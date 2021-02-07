// Copyright 2018 The conprof Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/conprof/db/storage"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/discovery"
	_ "github.com/prometheus/prometheus/discovery/install" // Register service discovery implementations.
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/prober"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/conprof/conprof/config"
	"github.com/conprof/conprof/pkg/store"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/conprof/scrape"
)

type perRequestBearerToken struct {
	token    string
	insecure bool
}

func (t *perRequestBearerToken) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (t *perRequestBearerToken) RequireTransportSecurity() bool {
	return !t.insecure
}

// registerSampler registers a sampler command.
func registerSampler(m map[string]setupFunc, app *kingpin.Application, name string, reloadCh chan struct{}, reloaders *configReloaders) {
	cmd := app.Command(name, "Run a sampler, that appends profiles to a configured storage.")

	configFile := cmd.Flag("config.file", "Config file to use.").
		Default("conprof.yaml").String()
	targets := cmd.Flag("target", "Targets to scrape.").Strings()
	storeAddress := cmd.Flag("store", "Address of statically configured store.").
		Default("127.0.0.1:10901").String()
	bearerToken := cmd.Flag("bearer-token", "Bearer token to authenticate with store.").String()
	bearerTokenFile := cmd.Flag("bearer-token-file", "File to read bearer token from to authenticate with store.").String()
	insecure := cmd.Flag("insecure", "Send gRPC requests via plaintext instead of TLS.").Default("false").Bool()
	insecureSkipVerify := cmd.Flag("insecure-skip-verify", "Skip TLS certificate verification.").Default("false").Bool()
	CAFile := cmd.Flag("caFile", "CA file path for TLS connection").Default("").String()
	CertFile := cmd.Flag("certFile", "CA file path for TLS connection").Default("").String()
	KeyFile := cmd.Flag("keyFile", "CA file path for TLS connection").Default("").String()

	m[name] = func(comp component.Component, g *run.Group, mux httpMux, probe prober.Probe, logger log.Logger, reg *prometheus.Registry, debugLogging bool) (prober.Probe, error) {
		met := grpc_prometheus.NewClientMetrics()
		met.EnableClientHandlingTimeHistogram()
		reg.MustRegister(met)

		opts := []grpc.DialOption{
			grpc.WithUnaryInterceptor(
				met.UnaryClientInterceptor(),
			),
		}
		if *insecure {
			opts = append(opts, grpc.WithInsecure())
		} else {
			config := &tls.Config{
				InsecureSkipVerify: *insecureSkipVerify,
			}
			if *CAFile != "" {
				cafile_data, err := ioutil.ReadFile(*CAFile)
				if err != nil {
					return nil, fmt.Errorf("unable to load specified CA cert %s: %s", CAFile, err)
				}
				caCertPool := x509.NewCertPool()
				if !caCertPool.AppendCertsFromPEM(cafile_data) {
					return nil, fmt.Errorf("unable to append CA file to cert pool")
				}
				config.RootCAs = caCertPool
			}

			// If a client cert & key is provided then configure TLS config accordingly.
			if *CertFile != "" && *KeyFile == "" {
				return nil, fmt.Errorf("client cert file %q specified without client key file", CertFile)
			} else if *KeyFile != "" && *CertFile == "" {
				return nil, fmt.Errorf("client key file %q specified without client cert file", KeyFile)
			} else if *CertFile != "" && *KeyFile != "" {
				cert, err := tls.LoadX509KeyPair(*CertFile, *KeyFile)
				if err != nil {
					return nil, fmt.Errorf("unable to use specified client cert (%s) & key (%s): %s", CertFile, KeyFile, err)
				}
				config.Certificates = []tls.Certificate{cert}
			}

			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(config)))
		}

		if bearerToken != nil && *bearerToken != "" {
			opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
				token:    *bearerToken,
				insecure: *insecure,
			}))
		}

		if bearerTokenFile != nil && *bearerTokenFile != "" {
			b, err := ioutil.ReadFile(*bearerTokenFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read bearer token from file: %w", err)
			}
			opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
				token:    string(b),
				insecure: *insecure,
			}))
		}

		conn, err := grpc.Dial(*storeAddress, opts...)
		if err != nil {
			return probe, err
		}
		c := storepb.NewWritableProfileStoreClient(conn)
		return probe, runSampler(g, probe, logger, store.NewGRPCAppendable(logger, c), *configFile, *targets, reloadCh, reloaders)
	}
}

func getScrapeConfigs(cfg *config.Config) map[string]discovery.Configs {
	c := make(map[string]discovery.Configs)
	for _, v := range cfg.ScrapeConfigs {
		c[v.JobName] = v.ServiceDiscoveryConfigs
	}
	return c
}

func managerReloader(logger log.Logger, reloadCh chan struct{}, configFile string, reloaders *configReloaders) {
	for {
		<-reloadCh
		level.Info(logger).Log("msg", "Reloading configuration")
		cfg, err := config.LoadFile(configFile)
		if err != nil {
			level.Error(logger).Log("could not load config to reload: %v", err)
		}

		for _, reloader := range reloaders.funcs {
			if err := reloader(cfg); err != nil {
				level.Error(logger).Log("could not reload scrape configs: %v", err)
			}
		}
	}
}

func runSampler(
	g *run.Group,
	probe prober.Probe,
	logger log.Logger,
	db storage.Appendable,
	configFile string,
	targets []string,
	reloadCh chan struct{},
	reloaders *configReloaders,
) error {
	scrapeManager := scrape.NewManager(log.With(logger, "component", "scrape-manager"), db)
	reloaders.Register(scrapeManager.ApplyConfig)

	var (
		cfg *config.Config
		err error
	)
	if len(targets) > 0 {
		targetStrings := []string{}
		for _, t := range targets {
			targetStrings = append(targetStrings, fmt.Sprintf("\"%s\"", t))
		}
		tmpfile, err := ioutil.TempFile("", "conprof")
		if err != nil {
			return fmt.Errorf("could not create tempfile: %v", err)
		}

		content := fmt.Sprintf(`
scrape_configs:
- job_name: 'default'
  scrape_interval: 1m
  scrape_timeout: 1m
  static_configs:
  - targets: [%s]
`, strings.Join(targetStrings, ","))
		if _, err := tmpfile.Write([]byte(content)); err != nil {
			return fmt.Errorf("could write tempfile: %v", err)
		}
		if err := tmpfile.Close(); err != nil {
			return fmt.Errorf("could close tempfile: %v", err)
		}

		configFile = tmpfile.Name()
		cfg, err = config.LoadFile(configFile)
		if err != nil {
			return fmt.Errorf("could not load config: %v", err)
		}
	} else {
		cfg, err = config.LoadFile(configFile)
		if err != nil {
			return fmt.Errorf("could not load config: %v", err)
		}
	}

	ctxScrape, cancelScrape := context.WithCancel(context.Background())
	discoveryManagerScrape := discovery.NewManager(ctxScrape, log.With(logger, "component", "discovery manager scrape"), discovery.Name("scrape"))

	reloaders.Register(func(cfg *config.Config) error {
		c := getScrapeConfigs(cfg)
		for _, v := range cfg.ScrapeConfigs {
			c[v.JobName] = v.ServiceDiscoveryConfigs
		}
		return discoveryManagerScrape.ApplyConfig(c)
	})

	go managerReloader(logger, reloadCh, configFile, reloaders)
	{
		err := discoveryManagerScrape.ApplyConfig(getScrapeConfigs(cfg))
		if err != nil {
			level.Error(logger).Log("msg", err)
			cancelScrape()
			return err
		}
		// Scrape discovery manager.
		g.Add(
			func() error {
				err := discoveryManagerScrape.Run()
				level.Info(logger).Log("msg", "Scrape discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping scrape discovery manager...")
				cancelScrape()
			},
		)
	}
	{
		_, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			err = scrapeManager.ApplyConfig(cfg)
			if err != nil {
				return fmt.Errorf("could not apply config: %v", err)
			}
			return scrapeManager.Run(discoveryManagerScrape.SyncCh())
		}, func(error) {
			level.Debug(logger).Log("msg", "shutting down scrape manager")
			scrapeManager.Stop()
			cancel()
		})
	}

	probe.Ready()
	return nil
}
