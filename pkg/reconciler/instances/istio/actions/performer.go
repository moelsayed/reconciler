package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/kyma-incubator/reconciler/pkg/reconciler/chart"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/clientset"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/istioctl"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/manifest"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/reset/proxy"
	"github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes"
	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgo "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	istioConfig "github.com/kyma-incubator/reconciler/pkg/reconciler/instances/istio/reset/config"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	helmChart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

const (
	istioImagePrefix    = "istio/proxyv2"
	retriesCount        = 5
	delayBetweenRetries = 5 * time.Second
	timeout             = 5 * time.Minute
	interval            = 12 * time.Second
)

type VersionType string

type IstioStatus struct {
	ClientVersion    string
	TargetVersion    string
	PilotVersion     string
	DataPlaneVersion string
}

type IstioVersionOutput struct {
	ClientVersion    *ClientVersion      `json:"clientVersion"`
	MeshVersion      []*MeshComponent    `json:"meshVersion,omitempty"`
	DataPlaneVersion []*DataPlaneVersion `json:"dataPlaneVersion,omitempty"`
}

type ClientVersion struct {
	Version string `json:"version"`
}

type MeshComponent struct {
	Component string    `json:"Component,omitempty"`
	Info      *MeshInfo `json:"Info,omitempty"`
}

type MeshInfo struct {
	Version string `json:"version,omitempty"`
}

type DataPlaneVersion struct {
	IstioVersion string `json:"IstioVersion,omitempty"`
}

type chartValues struct {
	Global struct {
		Images struct {
			IstioPilot struct {
				Version string `json:"version"`
			} `json:"istio_pilot"`
		} `json:"images"`
	} `json:"global"`
}

//go:generate mockery --name=IstioPerformer --outpkg=mock --case=underscore
// IstioPerformer performs actions on Istio component on the cluster.
type IstioPerformer interface {

	// Install Istio in given version on the cluster using istioChart.
	Install(kubeConfig, istioChart, version string, logger *zap.SugaredLogger) error

	// PatchMutatingWebhook patches Istio's webhook configuration.
	PatchMutatingWebhook(ctx context.Context, kubeClient kubernetes.Client, logger *zap.SugaredLogger) error

	// Update Istio on the cluster to the targetVersion using istioChart.
	Update(kubeConfig, istioChart, targetVersion string, logger *zap.SugaredLogger) error

	// ResetProxy resets Istio proxy of all Istio sidecars on the cluster. The proxyImageVersion parameter controls the Istio proxy version, it always adds "-distroless" suffix to the provided value.
	ResetProxy(context context.Context, kubeConfig string, proxyImageVersion string, logger *zap.SugaredLogger) error

	// Version reports status of Istio installation on the cluster.
	Version(workspace chart.Factory, branchVersion string, istioChart string, kubeConfig string, logger *zap.SugaredLogger) (IstioStatus, error)

	// Uninstall Istio from the cluster and its corresponding resources, using given Istio version.
	Uninstall(kubeClientSet kubernetes.Client, version string, logger *zap.SugaredLogger) error
}

// CommanderResolver interface implementations must be able to provide istioctl.Commander instances for given istioctl.Version
type CommanderResolver interface {
	// GetCommander function returns istioctl.Commander instance for given istioctl version if supported, returns an error otherwise.
	GetCommander(version istioctl.Version) (istioctl.Commander, error)
}

// DefaultIstioPerformer provides a default implementation of IstioPerformer.
// It uses istioctl binary to do it's job. It delegates the job of finding proper istioctl binary for given operation to the configured CommandResolver.
type DefaultIstioPerformer struct {
	resolver        CommanderResolver
	istioProxyReset proxy.IstioProxyReset
	provider        clientset.Provider
}

// NewDefaultIstioPerformer creates a new instance of the DefaultIstioPerformer.
func NewDefaultIstioPerformer(resolver CommanderResolver, istioProxyReset proxy.IstioProxyReset, provider clientset.Provider) *DefaultIstioPerformer {
	return &DefaultIstioPerformer{resolver, istioProxyReset, provider}
}

func (c *DefaultIstioPerformer) Uninstall(kubeClientSet kubernetes.Client, version string, logger *zap.SugaredLogger) error {
	logger.Debug("Starting Istio uninstallation...")

	execVersion, err := istioctl.VersionFromString(version)
	if err != nil {
		return errors.Wrap(err, "Error parsing version")
	}

	commander, err := c.resolver.GetCommander(execVersion)
	if err != nil {
		return err
	}

	err = commander.Uninstall(kubeClientSet.Kubeconfig(), logger)
	if err != nil {
		return errors.Wrap(err, "Error occurred when calling istioctl")
	}
	logger.Debug("Istio uninstall triggered")
	kubeClient, err := kubeClientSet.Clientset()
	if err != nil {
		return err
	}

	policy := metav1.DeletePropagationForeground
	err = kubeClient.CoreV1().Namespaces().Delete(context.TODO(), "istio-system", metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		return err
	}
	logger.Debug("Istio namespace deleted")
	return nil
}

func (c *DefaultIstioPerformer) Install(kubeConfig, istioChart, version string, logger *zap.SugaredLogger) error {
	logger.Debug("Starting Istio installation...")

	execVersion, err := istioctl.VersionFromString(version)
	if err != nil {
		return errors.Wrap(err, "Error parsing version")
	}

	istioOperatorManifest, err := manifest.ExtractIstioOperatorContextFrom(istioChart)
	if err != nil {
		return err
	}

	commander, err := c.resolver.GetCommander(execVersion)
	if err != nil {
		return err
	}

	err = commander.Install(istioOperatorManifest, kubeConfig, logger)
	if err != nil {
		return errors.Wrap(err, "Error occurred when calling istioctl")
	}
	logger.Infof("Istio in version %s successfully installed", version)
	return nil
}

func (c *DefaultIstioPerformer) PatchMutatingWebhook(context context.Context, kubeClient kubernetes.Client, logger *zap.SugaredLogger) error {
	clientSet, err := kubeClient.Clientset()
	if err != nil {
		return err
	}

	const primary = "istio-revision-tag-default"
	const secondary = "istio-sidecar-injector"
	candidatesNames := []string{primary, secondary}

	webhookNameToChange := "auto.sidecar-injector.istio.io"

	requiredLabelSelector := metav1.LabelSelectorRequirement{
		Key:      "gardener.cloud/purpose",
		Operator: "NotIn",
		Values:   []string{"kube-system"},
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		whConf, err := c.selectWebhookConfFormCandidates(context, candidatesNames, clientSet)
		if err != nil {
			return err
		}
		err = c.addNamespaceSelectorIfNotPresent(whConf, webhookNameToChange, requiredLabelSelector)
		if err != nil {
			return err
		}
		_, err = clientSet.AdmissionregistrationV1().
			MutatingWebhookConfigurations().
			Update(context, whConf, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}

	logger.Debugf("Patch has been applied successfully")

	return nil
}

func (c *DefaultIstioPerformer) addNamespaceSelectorIfNotPresent(whConf *v1.MutatingWebhookConfiguration, webhookNameToChange string, requiredLabelSelector metav1.LabelSelectorRequirement) error {
	for i := range whConf.Webhooks {
		if whConf.Webhooks[i].Name == webhookNameToChange {
			matchExpressions := whConf.Webhooks[i].NamespaceSelector.MatchExpressions
			var hasRequiredLabel bool
			for j := range matchExpressions {
				if hasRequiredLabel = reflect.DeepEqual(matchExpressions[j], requiredLabelSelector); hasRequiredLabel {
					break
				}
			}
			if !hasRequiredLabel {
				matchExpressions = append(matchExpressions, requiredLabelSelector)
				whConf.Webhooks[i].NamespaceSelector.MatchExpressions = matchExpressions
			}
			return nil
		}
	}
	return fmt.Errorf("could not find webhook %s in WebhookConfiguration %s", webhookNameToChange, whConf.Name)
}

func (c *DefaultIstioPerformer) selectWebhookConfFormCandidates(context context.Context, candidatesNames []string, clientSet clientgo.Interface) (wh *v1.MutatingWebhookConfiguration, err error) {
	for _, webhookName := range candidatesNames {
		wh, err = clientSet.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context, webhookName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		return
	}
	return nil, errors.Wrap(err, "MutatingWebhookConfigurations could not be selected from candidates")
}

func (c *DefaultIstioPerformer) Update(kubeConfig, istioChart, targetVersion string, logger *zap.SugaredLogger) error {
	logger.Debug("Starting Istio update...")

	version, err := istioctl.VersionFromString(targetVersion)
	if err != nil {
		return errors.Wrap(err, "Error parsing version")
	}

	istioOperatorManifest, err := manifest.ExtractIstioOperatorContextFrom(istioChart)
	if err != nil {
		return err
	}

	commander, err := c.resolver.GetCommander(version)
	if err != nil {
		return err
	}

	err = commander.Upgrade(istioOperatorManifest, kubeConfig, logger)
	if err != nil {
		return errors.Wrap(err, "Error occurred when calling istioctl")
	}

	logger.Infof("Istio has been updated successfully to version %s", targetVersion)

	return nil
}

func (c *DefaultIstioPerformer) ResetProxy(context context.Context, kubeConfig string, proxyImageVersion string, logger *zap.SugaredLogger) error {
	kubeClient, err := c.provider.RetrieveFrom(kubeConfig, logger)
	if err != nil {
		logger.Error("Could not retrieve KubeClient from Kubeconfig!")
		return err
	}

	cfg := istioConfig.IstioProxyConfig{
		Context:             context,
		ImagePrefix:         istioImagePrefix,
		ImageVersion:        fmt.Sprintf("%s-distroless", proxyImageVersion),
		RetriesCount:        retriesCount,
		DelayBetweenRetries: delayBetweenRetries,
		Timeout:             timeout,
		Interval:            interval,
		Kubeclient:          kubeClient,
		Debug:               false,
		Log:                 logger,
	}

	err = c.istioProxyReset.Run(cfg)
	if err != nil {
		return errors.Wrap(err, "Istio proxy reset error")
	}

	return nil
}

func (c *DefaultIstioPerformer) Version(workspace chart.Factory, branchVersion string, istioChart string, kubeConfig string, logger *zap.SugaredLogger) (IstioStatus, error) {
	targetVersion, err := getTargetVersionFromIstioChart(workspace, branchVersion, istioChart, logger)
	if err != nil {
		return IstioStatus{}, errors.Wrap(err, "Target Version could not be found")
	}

	version, err := istioctl.VersionFromString(targetVersion)
	if err != nil {
		return IstioStatus{}, errors.Wrap(err, "Error parsing version")
	}

	commander, err := c.resolver.GetCommander(version)
	if err != nil {
		return IstioStatus{}, err
	}

	versionOutput, err := commander.Version(kubeConfig, logger)
	if err != nil {
		return IstioStatus{}, err
	}

	mappedIstioVersion, err := mapVersionToStruct(versionOutput, targetVersion)

	return mappedIstioVersion, err
}

func getTargetVersionFromIstioChart(workspace chart.Factory, branch string, istioChart string, logger *zap.SugaredLogger) (string, error) {
	ws, err := workspace.Get(branch)
	if err != nil {
		return "", err
	}

	istioHelmChart, err := loader.Load(filepath.Join(ws.ResourceDir, istioChart))
	if err != nil {
		return "", err
	}

	pilotVersion, err := getTargetVersionFromPilotInChartValues(istioHelmChart)
	if err != nil {
		return "", err
	}

	if pilotVersion != "" {
		logger.Debugf("Resolved target Istio version: %s from values", pilotVersion)
		return pilotVersion, nil
	}

	appVersion := getTargetVersionFromAppVersionInChartDefinition(istioHelmChart)
	if appVersion != "" {
		logger.Debugf("Resolved target Istio version: %s from Chart definition", appVersion)
		return appVersion, nil
	}

	return "", errors.New("Target Istio version could not be found neither in Chart.yaml nor in helm values")
}

func getTargetVersionFromAppVersionInChartDefinition(helmChart *helmChart.Chart) string {
	return helmChart.Metadata.AppVersion
}

func getTargetVersionFromPilotInChartValues(helmChart *helmChart.Chart) (string, error) {
	mapAsJSON, err := json.Marshal(helmChart.Values)
	if err != nil {
		return "", err
	}

	var chartValues chartValues
	err = json.Unmarshal(mapAsJSON, &chartValues)
	if err != nil {
		return "", err
	}

	return chartValues.Global.Images.IstioPilot.Version, nil
}

func getVersionFromJSON(versionType VersionType, json IstioVersionOutput) string {
	switch versionType {
	case "client":
		return json.ClientVersion.Version
	case "pilot":
		if len(json.MeshVersion) > 0 {
			return json.MeshVersion[0].Info.Version
		}
		return ""
	case "dataPlane":
		if len(json.DataPlaneVersion) > 0 {
			return json.DataPlaneVersion[0].IstioVersion
		}
		return ""
	default:
		return ""
	}
}

func mapVersionToStruct(versionOutput []byte, targetVersion string) (IstioStatus, error) {
	if len(versionOutput) == 0 {
		return IstioStatus{}, errors.New("the result of the version command is empty")
	}

	if index := bytes.IndexRune(versionOutput, '{'); index != 0 {
		versionOutput = versionOutput[bytes.IndexRune(versionOutput, '{'):]
	}

	var version IstioVersionOutput
	err := json.Unmarshal(versionOutput, &version)

	if err != nil {
		return IstioStatus{}, err
	}

	return IstioStatus{
		ClientVersion:    getVersionFromJSON("client", version),
		TargetVersion:    targetVersion,
		PilotVersion:     getVersionFromJSON("pilot", version),
		DataPlaneVersion: getVersionFromJSON("dataPlane", version),
	}, nil
}
