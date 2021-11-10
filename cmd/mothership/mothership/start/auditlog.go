package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/kyma-incubator/reconciler/pkg/keb"
	"github.com/kyma-incubator/reconciler/pkg/server"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	XJWTHeaderName = "X-Jwt"
)

func NewLoggerWithFile(logFile string) (*zap.Logger, error) {
	cfg := zap.Config{
		Encoding:         "json",
		Level:            zap.NewAtomicLevelAt(zapcore.DebugLevel),
		OutputPaths:      []string{logFile},
		ErrorOutputPaths: []string{logFile},
	}

	logger, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	ws := zapcore.AddSync(&lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    100, // megabytes
		MaxBackups: 5,
		MaxAge:     14,    // days
		Compress:   false, // save cpu cycles
	})
	// I need to replace the default core logger whit a new one that contains
	// WriterSyncer that wraps luberjack. Lumberjack handels the log rotation.
	return logger.WithOptions(
		zap.WrapCore(func(zapcore.Core) zapcore.Core {
			return zapcore.NewCore(
				zapcore.NewJSONEncoder(zapcore.EncoderConfig{
					MessageKey:   "",
					LevelKey:     "level",
					EncodeLevel:  zapcore.CapitalLevelEncoder,
					TimeKey:      "time",
					EncodeTime:   zapcore.ISO8601TimeEncoder,
					EncodeCaller: zapcore.ShortCallerEncoder,
				}),
				ws,
				zap.InfoLevel,
			)
		}),
	), err
}

func NewAuditLoggerMiddelware(l *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auditLogRequest(w, r, l)

			next.ServeHTTP(w, r)
		})
	}
}

type data struct {
	ContractVersion int64  `json:"contractVersion"`
	Method          string `json:"method"`
	URI             string `json:"uri"`
	RequestBody     string `json:"requestBody"`
	User            string `json:"user"`
	JWTPayload      string `json:"jwtPayload"`
}

func auditLogRequest(w http.ResponseWriter, r *http.Request, l *zap.Logger) {
	params := server.NewParams(r)
	contractV, err := params.Int64(paramContractVersion)
	if err != nil {

		server.SendHTTPError(w, http.StatusBadRequest, &keb.HTTPErrorResponse{
			Error: errors.Wrap(err, "Contract version undefined").Error(),
		})
		return
	}
	logData := data{
		ContractVersion: contractV,
		Method:          r.Method,
		URI:             r.RequestURI,
		User:            "UNKOWEN_USER",
	}
	if jwtPayload, err := getJWTPayload(r); err == nil {
		logData.JWTPayload = jwtPayload
	} else {
		server.SendHTTPError(w, http.StatusInternalServerError, &keb.HTTPErrorResponse{
			Error: errors.Wrap(err, fmt.Sprintf("Failed to parse %s header content ", XJWTHeaderName)).Error(),
		})
		return
	}
	if user, err := getJWTPayloadSub(logData.JWTPayload); err == nil {
		if user != "" {
			logData.User = user
		}
	} else {
		server.SendHTTPError(w, http.StatusInternalServerError, &keb.HTTPErrorResponse{
			Error: errors.Wrap(err, "failed to Unmarshal JWT payload").Error(),
		})
		return
	}

	// log request body if needed.
	if r.Method == "POST" || r.Method == "PUT" {
		reqBody, err := ioutil.ReadAll(r.Body)
		if err != nil {
			server.SendHTTPError(w, http.StatusInternalServerError, &keb.HTTPErrorResponse{
				Error: errors.Wrap(err, "Failed to read received JSON payload").Error(),
			})
			return
		}
		r.Body = ioutil.NopCloser(bytes.NewBuffer(reqBody))
		logData.RequestBody = string(reqBody)
	}
	data, err := json.Marshal(logData)
	if err != nil {
		server.SendHTTPError(w, http.StatusInternalServerError, &keb.HTTPErrorResponse{
			Error: errors.Wrap(err, "Failed to marshal auditlog JSON payload").Error(),
		})
		return
	}
	l.With(zap.String("time", time.Now().Format(time.RFC3339))).
		With(zap.String("uuid", uuid.New().String())).
		With(zap.String("user", logData.User)).
		With(zap.String("data", string(data))).
		Info("")
}

func getJWTPayload(r *http.Request) (string, error) {
	jwtHeader := r.Header.Get(XJWTHeaderName)
	if len(jwtHeader) == 0 {
		return "", nil
	}
	decodedSeg, err := base64.RawURLEncoding.DecodeString(jwtHeader)
	return string(decodedSeg), err
}

type jwtSub struct {
	Sub string `json:"sub"`
}

func getJWTPayloadSub(payload string) (string, error) {
	if payload == "" {
		return "", nil
	}
	s := jwtSub{}
	err := json.Unmarshal([]byte(payload), &s)
	return s.Sub, err
}
