// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore/pkg/signature"

	"chainguard.dev/go-grpc-kit/pkg/duplex"
	ctclient "github.com/google/certificate-transparency-go/client"
	"github.com/google/certificate-transparency-go/jsonclient"
	grpcmw "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	certauth "github.com/sigstore/fulcio/pkg/ca"
	"github.com/sigstore/fulcio/pkg/ca/ephemeralca"
	"github.com/sigstore/fulcio/pkg/ca/fileca"
	googlecav1 "github.com/sigstore/fulcio/pkg/ca/googleca/v1"
	"github.com/sigstore/fulcio/pkg/ca/kmsca"
	"github.com/sigstore/fulcio/pkg/ca/pkcs11ca"
	"github.com/sigstore/fulcio/pkg/ca/tinkca"
	"github.com/sigstore/fulcio/pkg/config"
	"github.com/sigstore/fulcio/pkg/generated/protobuf"
	"github.com/sigstore/fulcio/pkg/generated/protobuf/legacy"
	"github.com/sigstore/fulcio/pkg/identity"
	"github.com/sigstore/fulcio/pkg/log"
	"github.com/sigstore/fulcio/pkg/server"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"goa.design/goa/v3/grpc/middleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	health "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

const (
	serveCmdEnvPrefix        = "FULCIO_SERVE"
	defaultConfigPath string = "/etc/fulcio-config/config.yaml"
)

var serveCmdConfigFilePath string

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "start http server with configured api",
		Long:  `Starts a http server and serves the configured api`,
		Run:   runServeCmd,
	}

	cmd.Flags().StringVarP(&serveCmdConfigFilePath, "config", "c", "", "config file containing all settings")
	cmd.Flags().String("log_type", "dev", "logger type to use (dev/prod)")
	cmd.Flags().String("ca", "", "googleca | tinkca | pkcs11ca | fileca | kmsca | ephemeralca (for testing)")
	cmd.Flags().String("aws-hsm-root-ca-path", "", "Path to root CA on disk (only used with AWS HSM)")
	cmd.Flags().String("gcp_private_ca_parent", "", "private ca parent: projects/<project>/locations/<location>/caPools/<caPool> (only used with --ca googleca)"+
		"Optionally specify /certificateAuthorities/<caID>, which will bypass CA pool load balancing.")
	cmd.Flags().String("hsm-caroot-id", "", "HSM ID for Root CA (only used with --ca pkcs11ca)")
	cmd.Flags().String("ct-log-url", "http://localhost:6962/test", "host and path (with log prefix at the end) to the ct log")
	cmd.Flags().String("ct-log-public-key-path", "", "Path to a PEM-encoded public key of the CT log, used to verify SCTs")
	cmd.Flags().String("config-path", defaultConfigPath, "path to fulcio config yaml")
	cmd.Flags().String("pkcs11-config-path", "config/crypto11.conf", "path to fulcio pkcs11 config file")
	cmd.Flags().String("fileca-cert", "", "Path to CA certificate")
	cmd.Flags().String("fileca-key", "", "Path to CA encrypted private key")
	cmd.Flags().String("fileca-key-passwd", "", "Password to decrypt CA private key")
	cmd.Flags().Bool("fileca-watch", true, "Watch filesystem for updates")
	cmd.Flags().String("kms-resource", "", "KMS key resource path. Must be prefixed with awskms://, azurekms://, gcpkms://, or hashivault://")
	cmd.Flags().String("kms-cert-chain-path", "", "Path to PEM-encoded CA certificate chain for KMS-backed CA")
	cmd.Flags().String("tink-kms-resource", "", "KMS key resource path for encrypted Tink keyset. Must be prefixed with gcp-kms:// or aws-kms://")
	cmd.Flags().String("tink-cert-chain-path", "", "Path to PEM-encoded CA certificate chain for Tink-backed CA")
	cmd.Flags().String("tink-keyset-path", "", "Path to KMS-encrypted keyset for Tink-backed CA")
	cmd.Flags().String("host", "0.0.0.0", "The host on which to serve requests for HTTP; --http-host is alias")
	cmd.Flags().String("port", "8080", "The port on which to serve requests for HTTP; --http-port is alias")
	cmd.Flags().String("grpc-host", "0.0.0.0", "The host on which to serve requests for GRPC")
	cmd.Flags().String("grpc-port", "8081", "The port on which to serve requests for GRPC")
	cmd.Flags().String("metrics-port", "2112", "The port on which to serve prometheus metrics endpoint")
	cmd.Flags().String("legacy-unix-domain-socket", LegacyUnixDomainSocket, "The Unix domain socket used for the legacy gRPC server")
	cmd.Flags().Duration("read-header-timeout", 10*time.Second, "The time allowed to read the headers of the requests in seconds")
	cmd.Flags().String("grpc-tls-certificate", "", "the certificate file to use for secure connections - only applies to grpc-port")
	cmd.Flags().String("grpc-tls-key", "", "the private key file to use for secure connections (without passphrase) - only applies to grpc-port")
	cmd.Flags().Duration("idle-connection-timeout", 30*time.Second, "The time allowed for connections (HTTP or gRPC) to go idle before being closed by the server")
	cmd.Flags().String("ct-log.tls-ca-cert", "", "Path to TLS CA certificate used to connect to ct-log")
	cmd.Flags().StringSlice("client-signing-algorithms", buildDefaultClientSigningAlgorithms([]v1.PublicKeyDetails{
		v1.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256,
		v1.PublicKeyDetails_PKIX_ECDSA_P384_SHA_384,
		v1.PublicKeyDetails_PKIX_ECDSA_P521_SHA_512,
		v1.PublicKeyDetails_PKIX_RSA_PKCS1V15_2048_SHA256,
		v1.PublicKeyDetails_PKIX_RSA_PKCS1V15_3072_SHA256,
		v1.PublicKeyDetails_PKIX_RSA_PKCS1V15_4096_SHA256,
		v1.PublicKeyDetails_PKIX_ED25519,
	}), "the list of allowed client signing algorithms")

	// convert "http-host" flag to "host" and "http-port" flag to be "port"
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "http-port":
			name = "port"
		case "http-host":
			name = "host"
		}
		return pflag.NormalizedName(name)
	})
	viper.RegisterAlias("http-host", "host")
	viper.RegisterAlias("http-port", "port")

	return cmd
}

const (
	maxMsgSize int64 = 1 << 22 // 4MiB
)

// Adaptor for logging with the CT log
type logAdaptor struct {
	logger *zap.SugaredLogger
}

func (la logAdaptor) Printf(s string, args ...interface{}) {
	la.logger.Infof(s, args...)
}

func runServeCmd(cmd *cobra.Command, args []string) { //nolint: revive
	ctx := cmd.Context()
	// If a config file is provided, modify the viper config to locate and read it
	if err := checkServeCmdConfigFile(); err != nil {
		log.Logger.Fatal(err)
	}

	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		log.Logger.Fatal(err)
	}

	// Allow recognition of environment variables such as FULCIO_SERVE_CA etc.
	viper.SetEnvPrefix(serveCmdEnvPrefix)
	viper.AutomaticEnv()

	switch viper.GetString("ca") {
	case "":
		log.Logger.Fatal("required flag \"ca\" not set")

	case "pkcs11ca":
		if !viper.IsSet("hsm-caroot-id") {
			log.Logger.Fatal("hsm-caroot-id must be set when using pkcs11ca")
		}

	case "googleca":
		if !viper.IsSet("gcp_private_ca_parent") {
			log.Logger.Fatal("gcp_private_ca_parent must be set when using googleca")
		}
		if viper.IsSet("gcp_private_ca_version") {
			// There's a MarkDeprecated function in cobra/pflags, but it doesn't use log.Logger
			log.Logger.Warn("gcp_private_ca_version is deprecated and will soon be removed; please remove it")
		}
	case "fileca":
		if !viper.IsSet("fileca-cert") {
			log.Logger.Fatal("fileca-cert must be set to certificate path when using fileca")
		}
		if !viper.IsSet("fileca-key") {
			log.Logger.Fatal("fileca-key must be set to private key path when using fileca")
		}
		if !viper.IsSet("fileca-key-passwd") {
			log.Logger.Fatal("fileca-key-passwd must be set to encryption password for private key file when using fileca")
		}
	case "kmsca":
		if !viper.IsSet("kms-resource") {
			log.Logger.Fatal("kms-resource must be set when using kmsca")
		}
		if !viper.IsSet("kms-cert-chain-path") {
			log.Logger.Fatal("kms-cert-chain-path must be set when using kmsca")
		}
	case "tinkca":
		if !viper.IsSet("tink-kms-resource") {
			log.Logger.Fatal("tink-kms-resource must be set when using tinkca")
		}
		if !viper.IsSet("tink-cert-chain-path") {
			log.Logger.Fatal("tink-cert-chain-path must be set when using tinkca")
		}
		if !viper.IsSet("tink-keyset-path") {
			log.Logger.Fatal("tink-keyset-path must be set when using tinkca")
		}
	case "ephemeralca":
		// this is a no-op since this is a self-signed in-memory CA for testing
	default:
		log.Logger.Fatalf("--ca=%s is not a valid selection. Try: pkcs11ca, googleca, fileca, or ephemeralca", viper.GetString("ca"))
	}

	// Setup the logger to dev/prod
	log.ConfigureLogger(viper.GetString("log_type"))

	algorithmStrings := viper.GetStringSlice("client-signing-algorithms")
	var algorithmConfig []v1.PublicKeyDetails
	for _, s := range algorithmStrings {
		algorithmValue, err := signature.ParseSignatureAlgorithmFlag(s)
		if err != nil {
			log.Logger.Fatal(err)
		}
		algorithmConfig = append(algorithmConfig, algorithmValue)
	}
	algorithmRegistry, err := signature.NewAlgorithmRegistryConfig(algorithmConfig)
	if err != nil {
		log.Logger.Fatalf("error loading --client-signing-algorithms=%s: %v", algorithmConfig, err)
	}

	// from https://github.com/golang/glog/commit/fca8c8854093a154ff1eb580aae10276ad6b1b5f
	_ = flag.CommandLine.Parse([]string{})

	cp := viper.GetString("config-path")
	if cp == defaultConfigPath {
		if _, err := os.Stat(cp); os.IsNotExist(err) {
			log.Logger.Warnf("warn loading --config-path=%s: %v, fall back to json", cp, err)
			cp = strings.TrimSuffix(cp, ".yaml") + ".json"
		}
	}

	cfg, err := config.Load(cp)
	if err != nil {
		log.Logger.Fatalf("error loading --config-path=%s: %v", cp, err)
	}

	var baseca certauth.CertificateAuthority
	switch viper.GetString("ca") {
	case "googleca":
		baseca, err = googlecav1.NewCertAuthorityService(cmd.Context(), viper.GetString("gcp_private_ca_parent"))
	case "pkcs11ca":
		params := pkcs11ca.Params{
			ConfigPath: viper.GetString("pkcs11-config-path"),
			RootID:     viper.GetString("hsm-caroot-id"),
		}
		if viper.IsSet("aws-hsm-root-ca-path") {
			path := viper.GetString("aws-hsm-root-ca-path")
			params.CAPath = &path
		}
		baseca, err = pkcs11ca.NewPKCS11CA(params)
	case "fileca":
		certFile := viper.GetString("fileca-cert")
		keyFile := viper.GetString("fileca-key")
		keyPass := viper.GetString("fileca-key-passwd")
		watch := viper.GetBool("fileca-watch")
		baseca, err = fileca.NewFileCA(certFile, keyFile, keyPass, watch)
	case "ephemeralca":
		baseca, err = ephemeralca.NewEphemeralCA()
	case "kmsca":
		var data []byte
		data, err = os.ReadFile(filepath.Clean(viper.GetString("kms-cert-chain-path")))
		if err != nil {
			log.Logger.Fatalf("error reading the kms certificate chain from '%s': %v", viper.GetString("kms-cert-chain-path"), err)
		}
		var certs []*x509.Certificate
		certs, err = cryptoutils.LoadCertificatesFromPEM(bytes.NewReader(data))
		if err != nil {
			log.Logger.Fatalf("error loading the PEM certificates from the kms certificate chain from '%s': %v", viper.GetString("kms-cert-chain-path"), err)
		}
		baseca, err = kmsca.NewKMSCA(cmd.Context(), viper.GetString("kms-resource"), certs)
	case "tinkca":
		baseca, err = tinkca.NewTinkCA(cmd.Context(),
			viper.GetString("tink-kms-resource"), viper.GetString("tink-keyset-path"), viper.GetString("tink-cert-chain-path"))
	default:
		err = fmt.Errorf("invalid value for configured CA: %v", baseca)
	}
	if err != nil {
		log.Logger.Fatal(err)
	}
	defer baseca.Close()

	var ctClient *ctclient.LogClient
	if logURL := viper.GetString("ct-log-url"); logURL != "" {
		opts := jsonclient.Options{
			Logger: logAdaptor{logger: log.Logger},
		}
		// optionally add CT log public key to verify SCTs
		if pubKeyPath := viper.GetString("ct-log-public-key-path"); pubKeyPath != "" {
			pemPubKey, err := os.ReadFile(filepath.Clean(pubKeyPath))
			if err != nil {
				log.Logger.Fatal(err)
			}
			opts.PublicKey = string(pemPubKey)
		}
		var httpClient *http.Client
		if tlsCaCertPath := viper.GetString("ct-log.tls-ca-cert"); tlsCaCertPath != "" {
			tlsCaCert, err := os.ReadFile(filepath.Clean(tlsCaCertPath))
			if err != nil {
				log.Logger.Fatal(err)
			}
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM(tlsCaCert); !ok {
				log.Logger.Fatal("failed to append TLS CA certificate")
			}
			tlsConfig := &tls.Config{
				RootCAs:    caCertPool,
				MinVersion: tls.VersionTLS12,
			}
			transport := &http.Transport{
				TLSClientConfig: tlsConfig,
			}
			httpClient = &http.Client{
				Timeout:   30 * time.Second,
				Transport: transport,
			}
		} else {
			httpClient = &http.Client{
				Timeout: 30 * time.Second,
			}
		}
		ctClient, err = ctclient.New(logURL, httpClient, opts)
		if err != nil {
			log.Logger.Fatal(err)
		}
	}
	ip := server.NewIssuerPool(cfg)

	portsMatch := viper.GetString("port") == viper.GetString("grpc-port")
	hostsMatch := viper.GetString("host") == viper.GetString("grpc-host")
	if portsMatch && hostsMatch {
		port := viper.GetInt("port")
		metricsPort := viper.GetInt("metrics-port")
		// StartDuplexServer will always return an error, log fatally if it's non-nil
		if err := StartDuplexServer(ctx, cfg, ctClient, baseca, algorithmRegistry, viper.GetString("host"), port, metricsPort, ip); err != http.ErrServerClosed {
			log.Logger.Fatal(err)
		}
		return
	}

	// waiting for http and grpc servers to shutdown gracefully
	var wg sync.WaitGroup

	httpServerEndpoint := fmt.Sprintf("%v:%v", viper.GetString("http-host"), viper.GetString("http-port"))

	reg := prometheus.NewRegistry()

	grpcServer, err := createGRPCServer(cfg, ctClient, baseca, algorithmRegistry, ip)
	if err != nil {
		log.Logger.Fatal(err)
	}
	grpcServer.setupPrometheus(reg)
	grpcServer.startTCPListener(&wg)

	legacyGRPCServer, err := createLegacyGRPCServer(cfg, viper.GetString("legacy-unix-domain-socket"), grpcServer.caService)
	if err != nil {
		log.Logger.Fatal(err)
	}
	legacyGRPCServer.startUnixListener()

	httpServer := createHTTPServer(ctx, httpServerEndpoint, grpcServer, legacyGRPCServer)
	httpServer.startListener(&wg)

	readHeaderTimeout := viper.GetDuration("read-header-timeout")
	prom := http.Server{
		Addr:              fmt.Sprintf(":%v", viper.GetString("metrics-port")),
		Handler:           promhttp.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
		<-sigint

		// received an interrupt signal, shut down
		if err := prom.Shutdown(context.Background()); err != nil {
			// error from closing listeners, or context timeout
			log.Logger.Errorf("HTTP server Shutdown: %v", err)
		}
		close(idleConnsClosed)
		log.Logger.Info("stopped prom server")
	}()
	if err := prom.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Logger.Fatal(err)
	}
	<-idleConnsClosed
	log.Logger.Info("prom server shutdown")

	// wait for http and grpc servers to shutdown
	wg.Wait()
}

func checkServeCmdConfigFile() error {
	if serveCmdConfigFilePath != "" {
		if _, err := os.Stat(serveCmdConfigFilePath); err != nil {
			return fmt.Errorf("unable to stat config file provided: %w", err)
		}
		abspath, err := filepath.Abs(serveCmdConfigFilePath)
		if err != nil {
			return fmt.Errorf("unable to determine absolute path of config file provided: %w", err)
		}
		extWithDot := filepath.Ext(abspath)
		ext := strings.TrimPrefix(extWithDot, ".")
		var extIsValid bool
		for _, validExt := range viper.SupportedExts {
			if ext == validExt {
				extIsValid = true
				break
			}
		}
		if !extIsValid {
			return fmt.Errorf("config file must have one of the following extensions: %s", strings.Join(viper.SupportedExts, ", "))
		}
		viper.SetConfigName(strings.TrimSuffix(filepath.Base(abspath), extWithDot))
		viper.SetConfigType(ext)
		viper.AddConfigPath(filepath.Dir(serveCmdConfigFilePath))
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("unable to parse config file provided: %w", err)
		}
	}
	return nil
}

func duplexHealthz(_ context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
	cc, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return err
	}
	registerHealthz := runtime.WithHealthzEndpoint(health.NewHealthClient(cc))
	registerHealthz(mux)
	return nil
}

func StartDuplexServer(ctx context.Context, cfg *config.FulcioConfig, ctClient *ctclient.LogClient, baseca certauth.CertificateAuthority, algorithmRegistry *signature.AlgorithmRegistryConfig, host string, port, metricsPort int, ip identity.IssuerPool) error {
	logger, opts := log.SetupGRPCLogging()

	d := duplex.New(
		port,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: viper.GetDuration("idle-connection-timeout"),
		}),
		runtime.WithMetadata(extractOIDCTokenFromAuthHeader),
		grpc.UnaryInterceptor(grpcmw.ChainUnaryServer(
			grpc_recovery.UnaryServerInterceptor(grpc_recovery.WithRecoveryHandlerContext(panicRecoveryHandler)), // recovers from per-transaction panics elegantly, so put it first
			middleware.UnaryRequestID(middleware.UseXRequestIDMetadataOption(true), middleware.XRequestMetadataLimitOption(128)),
			grpc_zap.UnaryServerInterceptor(logger, opts...),
			PassFulcioConfigThruContext(cfg),
			grpc_prometheus.UnaryServerInterceptor,
		)),
		grpc.MaxRecvMsgSize(int(maxMsgSize)),
		runtime.WithForwardResponseOption(setResponseCodeModifier),
	)

	// GRPC server
	grpcCAServer := server.NewGRPCCAServer(ctClient, baseca, algorithmRegistry, ip)
	protobuf.RegisterCAServer(d.Server, grpcCAServer)
	if err := d.RegisterHandler(ctx, protobuf.RegisterCAHandlerFromEndpoint); err != nil {
		return fmt.Errorf("registering grpc ca handler: %w", err)
	}

	// Legacy server
	legacyGRPCCAServer := server.NewLegacyGRPCCAServer(grpcCAServer)
	legacy.RegisterCAServer(d.Server, legacyGRPCCAServer)
	if err := d.RegisterHandler(ctx, legacy.RegisterCAHandlerFromEndpoint); err != nil {
		return fmt.Errorf("registering legacy grpc ca handler: %w", err)
	}

	// Prometheus
	reg := prometheus.NewRegistry()
	grpcMetrics := grpc_prometheus.DefaultServerMetrics
	grpcMetrics.EnableHandlingTimeHistogram()
	reg.MustRegister(grpcMetrics, server.MetricLatency, server.RequestsCount)
	grpc_prometheus.Register(d.Server)

	// Healthz
	health.RegisterHealthServer(d.Server, grpcCAServer)
	if err := d.RegisterHandler(ctx, duplexHealthz); err != nil {
		return fmt.Errorf("registering healthz endpoint: %w", err)
	}

	// Register prometheus handle.
	d.RegisterListenAndServeMetrics(metricsPort, false)

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}
	logger.Info("Starting duplex server...")
	if err := d.Serve(ctx, lis); err != nil {
		return fmt.Errorf("duplex server: %w", err)
	}
	return nil
}

func buildDefaultClientSigningAlgorithms(allowedAlgorithms []v1.PublicKeyDetails) []string {
	var algorithmStrings []string
	for _, algorithm := range allowedAlgorithms {
		algorithmString, err := signature.FormatSignatureAlgorithmFlag(algorithm)
		if err != nil {
			log.Logger.Fatal(err)
		}
		algorithmStrings = append(algorithmStrings, algorithmString)
	}
	return algorithmStrings
}
