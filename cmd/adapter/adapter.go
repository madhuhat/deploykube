/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/golang/glog"
	basecmd "github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/cmd"
	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/provider"
	resmetrics "github.com/kubernetes-incubator/metrics-server/pkg/apiserver/generic"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/util/logs"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	prom "github.com/directxman12/k8s-prometheus-adapter/pkg/client"
	mprom "github.com/directxman12/k8s-prometheus-adapter/pkg/client/metrics"
	adaptercfg "github.com/directxman12/k8s-prometheus-adapter/pkg/config"
	cmprov "github.com/directxman12/k8s-prometheus-adapter/pkg/custom-provider"
	resprov "github.com/directxman12/k8s-prometheus-adapter/pkg/resourceprovider"
)

type PrometheusAdapter struct {
	basecmd.AdapterBase

	// PrometheusURL is the URL describing how to connect to Prometheus.  Query parameters configure connection options.
	PrometheusURL string
	// PrometheusAuthInCluster enables using the auth details from the in-cluster kubeconfig to connect to Prometheus
	PrometheusAuthInCluster bool
	// PrometheusAuthConf is the kubeconfig file that contains auth details used to connect to Prometheus
	PrometheusAuthConf string
	// PrometheusCAFile points to the file containing the ca-root for connecting with Prometheus
	PrometheusCAFile string
	// AdapterConfigFile points to the file containing the metrics discovery configuration.
	AdapterConfigFile string
	// MetricsRelistInterval is the interval at which to relist the set of available metrics
	MetricsRelistInterval time.Duration

	metricsConfig *adaptercfg.MetricsDiscoveryConfig
}

func (cmd *PrometheusAdapter) makePromClient() (prom.Client, error) {
	baseURL, err := url.Parse(cmd.PrometheusURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Prometheus URL %q: %v", baseURL, err)
	}

	var httpClient *http.Client

	if cmd.PrometheusCAFile != "" {
		prometheusCAClient, err := makePrometheusCAClient(cmd.PrometheusCAFile)
		if err != nil {
			return nil, err
		}
		httpClient = prometheusCAClient
		fmt.Println("successfully loaded ca file")
	} else {
		kubeconfigHTTPClient, err := makeKubeconfigHTTPClient(cmd.PrometheusAuthInCluster, cmd.PrometheusAuthConf)
		if err != nil {
			return nil, err
		}
		httpClient = kubeconfigHTTPClient
		fmt.Println("successfully using in cluster")
	}

	genericPromClient := prom.NewGenericAPIClient(httpClient, baseURL)
	instrumentedGenericPromClient := mprom.InstrumentGenericAPIClient(genericPromClient, baseURL.String())
	return prom.NewClientForAPI(instrumentedGenericPromClient), nil
}

func (cmd *PrometheusAdapter) addFlags() {
	cmd.Flags().StringVar(&cmd.PrometheusURL, "prometheus-url", cmd.PrometheusURL,
		"URL for connecting to Prometheus.")
	cmd.Flags().BoolVar(&cmd.PrometheusAuthInCluster, "prometheus-auth-incluster", cmd.PrometheusAuthInCluster,
		"use auth details from the in-cluster kubeconfig when connecting to prometheus.")
	cmd.Flags().StringVar(&cmd.PrometheusAuthConf, "prometheus-auth-config", cmd.PrometheusAuthConf,
		"kubeconfig file used to configure auth when connecting to Prometheus.")
	cmd.Flags().StringVar(&cmd.PrometheusCAFile, "prometheus-ca-file", cmd.PrometheusCAFile,
		"Optional CA file to use when connecting with Prometheus")
	cmd.Flags().StringVar(&cmd.AdapterConfigFile, "config", cmd.AdapterConfigFile,
		"Configuration file containing details of how to transform between Prometheus metrics "+
			"and custom metrics API resources")
	cmd.Flags().DurationVar(&cmd.MetricsRelistInterval, "metrics-relist-interval", cmd.MetricsRelistInterval, ""+
		"interval at which to re-list the set of all available metrics from Prometheus")
}

func (cmd *PrometheusAdapter) loadConfig() error {
	// load metrics discovery configuration
	if cmd.AdapterConfigFile == "" {
		return fmt.Errorf("no metrics discovery configuration file specified (make sure to use --config)")
	}
	metricsConfig, err := adaptercfg.FromFile(cmd.AdapterConfigFile)
	if err != nil {
		return fmt.Errorf("unable to load metrics discovery configuration: %v", err)
	}

	cmd.metricsConfig = metricsConfig

	return nil
}

func (cmd *PrometheusAdapter) makeProvider(promClient prom.Client, stopCh <-chan struct{}) (provider.CustomMetricsProvider, error) {
	if len(cmd.metricsConfig.Rules) == 0 {
		return nil, nil
	}

	// grab the mapper and dynamic client
	mapper, err := cmd.RESTMapper()
	if err != nil {
		return nil, fmt.Errorf("unable to construct RESTMapper: %v", err)
	}
	dynClient, err := cmd.DynamicClient()
	if err != nil {
		return nil, fmt.Errorf("unable to construct Kubernetes client: %v", err)
	}

	// extract the namers
	namers, err := cmprov.NamersFromConfig(cmd.metricsConfig, mapper)
	if err != nil {
		return nil, fmt.Errorf("unable to construct naming scheme from metrics rules: %v", err)
	}

	// construct the provider and start it
	cmProvider, runner := cmprov.NewPrometheusProvider(mapper, dynClient, promClient, namers, cmd.MetricsRelistInterval)
	runner.RunUntil(stopCh)

	return cmProvider, nil
}

func (cmd *PrometheusAdapter) addResourceMetricsAPI(promClient prom.Client) error {
	if cmd.metricsConfig.ResourceRules == nil {
		// bail if we don't have rules for setting things up
		return nil
	}

	mapper, err := cmd.RESTMapper()
	if err != nil {
		return err
	}

	provider, err := resprov.NewProvider(promClient, mapper, cmd.metricsConfig.ResourceRules)
	if err != nil {
		return fmt.Errorf("unable to construct resource metrics API provider: %v", err)
	}

	provCfg := &resmetrics.ProviderConfig{
		Node: provider,
		Pod:  provider,
	}
	informers, err := cmd.Informers()
	if err != nil {
		return err
	}

	server, err := cmd.Server()
	if err != nil {
		return err
	}

	if err := resmetrics.InstallStorage(provCfg, informers.Core().V1(), server.GenericAPIServer); err != nil {
		return err
	}

	return nil
}

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	// set up flags
	cmd := &PrometheusAdapter{
		PrometheusURL:         "https://localhost",
		MetricsRelistInterval: 10 * time.Minute,
	}
	cmd.Name = "prometheus-metrics-adapter"
	cmd.addFlags()
	cmd.Flags().AddGoFlagSet(flag.CommandLine) // make sure we get the glog flags
	cmd.Flags().Parse(os.Args)

	// make the prometheus client
	promClient, err := cmd.makePromClient()
	if err != nil {
		glog.Fatalf("unable to construct Prometheus client: %v", err)
	}

	// load the config
	if err := cmd.loadConfig(); err != nil {
		glog.Fatalf("unable to load metrics discovery config: %v", err)
	}

	// construct the provider
	cmProvider, err := cmd.makeProvider(promClient, wait.NeverStop)
	if err != nil {
		glog.Fatalf("unable to construct custom metrics provider: %v", err)
	}

	// attach the provider to the server, if it's needed
	if cmProvider != nil {
		cmd.WithCustomMetrics(cmProvider)
	}

	// attach resource metrics support, if it's needed
	if err := cmd.addResourceMetricsAPI(promClient); err != nil {
		glog.Fatalf("unable to install resource metrics API: %v", err)
	}

	// run the server
	if err := cmd.Run(wait.NeverStop); err != nil {
		glog.Fatalf("unable to run custom metrics adapter: %v", err)
	}
}

// makeKubeconfigHTTPClient constructs an HTTP for connecting with the given auth options.
func makeKubeconfigHTTPClient(inClusterAuth bool, kubeConfigPath string) (*http.Client, error) {
	// make sure we're not trying to use two different sources of auth
	if inClusterAuth && kubeConfigPath != "" {
		return nil, fmt.Errorf("may not use both in-cluster auth and an explicit kubeconfig at the same time")
	}

	// return the default client if we're using no auth
	if !inClusterAuth && kubeConfigPath == "" {
		return http.DefaultClient, nil
	}

	var authConf *rest.Config
	if kubeConfigPath != "" {
		var err error
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfigPath}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
		authConf, err = loader.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to construct  auth configuration from %q for connecting to Prometheus: %v", kubeConfigPath, err)
		}
	} else {
		var err error
		authConf, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to construct in-cluster auth configuration for connecting to Prometheus: %v", err)
		}
	}
	tr, err := rest.TransportFor(authConf)
	if err != nil {
		return nil, fmt.Errorf("unable to construct client transport for connecting to Prometheus: %v", err)
	}
	return &http.Client{Transport: tr}, nil
}

func makePrometheusCAClient(caFilename string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("failed to read system certificates: %v", err)
	}
	data, err := ioutil.ReadFile(caFilename)
	if err != nil {
		return nil, fmt.Errorf("failed to read prometheus-ca-file: %v", err)
	}
	if !pool.AppendCertsFromPEM(data) {
		log.Printf("warning: no certs found in prometheus-ca-file")
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}, nil
}
