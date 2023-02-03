package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bufbuild/connect-go"
	grpchealth "github.com/bufbuild/connect-grpchealth-go"
	grpcreflect "github.com/bufbuild/connect-grpcreflect-go"

	//	compress "github.com/klauspost/connect-compress"

	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	goipam "github.com/cicdteam/go-ipam"
	goipamapiv1 "github.com/cicdteam/go-ipam/api/v1"
	"github.com/cicdteam/go-ipam/api/v1/apiv1connect"
	"github.com/cicdteam/go-ipam/pkg/service"
)

func main() {

	app := &cli.App{
		Name:  "vxlan ipam server",
		Usage: "grpc server for vxlan ipam",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "grpc-server-endpoint",
				Value:   ":9090",
				Usage:   "gRPC server endpoint",
				EnvVars: []string{"GOIPAM_GRPC_SERVER_ENDPOINT"},
			},
			&cli.StringFlag{
				Name:    "cidr",
				Value:   "10.10.10.0/24",
				Usage:   "CIDR for IP address management",
				EnvVars: []string{"GOIPAM_CIDR"},
			},
			&cli.StringFlag{
				Name:    "log-level",
				Value:   "info",
				Usage:   "log-level can be one of error|warn|info|debug",
				EnvVars: []string{"GOIPAM_LOG_LEVEL"},
			},
			&cli.StringFlag{
				Name:    "configmap",
				Value:   "vxlan-ipam",
				Usage:   "k8s ConfigMap name where ipam data stored",
				EnvVars: []string{"GOIPAM_CONFIGMAP_NAME"},
			},
			&cli.BoolFlag{
				Name:    "cache",
				Usage:   "cache queries to k8s ConfigMap",
				Value:   true,
				EnvVars: []string{"GOIPAM_CONFIGMAP_CACHE"},
			},
		},
		Action: func(ctx *cli.Context) error {
			c := getConfig(ctx)
			configmap := ctx.String("configmap")
			cache := ctx.Bool("cache")
			c.Storage = goipam.NewConfigmap(configmap, cache)
			s := newServer(c)
			return s.Run()
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatalf("Error in cli: %v", err)
	}

}

type config struct {
	GrpcServerEndpoint string
	Log                *zap.SugaredLogger
	Storage            goipam.Storage
	ConfigMapName      string
	ConfigMapCache     bool
	Cidr               string
}

func getConfig(ctx *cli.Context) config {
	cfg := zap.NewProductionConfig()
	level, err := zap.ParseAtomicLevel(ctx.String("log-level"))
	if err != nil {
		log.Fatal(err)
	}
	cfg.Level = level
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zlog, err := cfg.Build()
	if err != nil {
		log.Fatal(err)
	}

	return config{
		GrpcServerEndpoint: ctx.String("grpc-server-endpoint"),
		Cidr:               ctx.String("cidr"),
		Log:                zlog.Sugar(),
		ConfigMapName:      ctx.String("configmap"),
		ConfigMapCache:     ctx.Bool("cache"),
	}
}

type server struct {
	c       config
	ipamer  goipam.Ipamer
	storage goipam.Storage
	log     *zap.SugaredLogger
}

func newServer(c config) *server {

	return &server{
		c:       c,
		ipamer:  goipam.NewWithStorage(c.Storage),
		storage: c.Storage,
		log:     c.Log,
	}
}
func (s *server) Run() error {
	s.log.Infow("starting vxlan ipam server", "cidr", s.c.Cidr, "backend", s.storage.Name(), "configmap name", s.c.ConfigMapName, "configmap cache", s.c.ConfigMapCache, "endpoint", s.c.GrpcServerEndpoint)
	mux := http.NewServeMux()

	compress1KB := connect.WithCompressMinBytes(1024)

	// The generated constructors return a path and a plain net/http
	// handler.
	mux.Handle(apiv1connect.NewIpamServiceHandler(
		service.New(s.log, s.ipamer),
		compress1KB,
	))

	mux.Handle(grpchealth.NewHandler(
		grpchealth.NewStaticChecker(apiv1connect.IpamServiceName),
		compress1KB,
	))

	mux.Handle(grpcreflect.NewHandlerV1(
		grpcreflect.NewStaticReflector(apiv1connect.IpamServiceName),
		compress1KB,
	))

	server := http.Server{
		Addr: s.c.GrpcServerEndpoint,
		Handler: h2c.NewHandler(
			mux,
			&http2.Server{},
		),
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       1 * time.Minute,
		WriteTimeout:      1 * time.Minute,
		MaxHeaderBytes:    8 * 1024, // 8KiB
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() error {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP listen and serve: %v", err)
			return err
		}
		return nil
	}()

	s.log.Infow("creating prefix", "cidr", s.c.Cidr)
	c := apiv1connect.NewIpamServiceClient(
		http.DefaultClient,
		"http://"+s.c.GrpcServerEndpoint,
		connect.WithGRPC(),
	)

	prefixes, err := c.ListPrefixes(context.Background(), connect.NewRequest(&goipamapiv1.ListPrefixesRequest{}))
	if err != nil {
		return err
	}

	prefixAlreadyCreated := false
	for _, p := range prefixes.Msg.Prefixes {
		if p.Cidr == s.c.Cidr {
			prefixAlreadyCreated = true
			s.log.Infow("prefix already present, skip prefix creation", "cidr", p.Cidr)
		}
	}

	if !prefixAlreadyCreated {
		result, err := c.CreatePrefix(context.Background(), connect.NewRequest(&goipamapiv1.CreatePrefixRequest{Cidr: s.c.Cidr}))
		if err != nil {
			return err
		}
		s.log.Infow("prefix created", "cidr", result.Msg.Prefix.Cidr)
	}

	<-signals
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown: %v", err) //nolint:gocritic
		return err
	}

	return nil
}
