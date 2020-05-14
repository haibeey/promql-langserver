// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.  // You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

var (
	// defining this global variable will avoid to initialized it each time
	// and it will crash immediatly the server during the initialization in case the version is not well defined
	requiredVersion = semver.MustParse("2.15.0") // nolint: gochecknoglobals
)

func buildGenericRoundTripper(connectionTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   connectionTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // nolint: gas, gosec
	}
}

func buildStatusRequest(prometheusURL string) (*http.Request, error) {
	finalURL, err := url.Parse(prometheusURL)
	if err != nil {
		return nil, err
	}

	// define the path of the buildInfo
	// using this way will remove any issue that could be caused by a wrong URL set by the user
	finalURL.Path = "/api/v1/status/buildinfo"

	httpRequest, err := http.NewRequest(http.MethodGet, finalURL.String(), nil)
	if err != nil {
		return nil, err
	}
	// set the accept content type
	httpRequest.Header.Set("Accept", "application/json")
	return httpRequest, nil
}

type buildInfoResponse struct {
	Status    string        `json:"status"`
	Data      buildInfoData `json:"data,omitempty"`
	ErrorType string        `json:"errorType,omitempty"`
	Error     string        `json:"error,omitempty"`
	Warnings  []string      `json:"warnings,omitempty"`
}

// buildInfoData contains build information about Prometheus.
type buildInfoData struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

type Client interface {
	Metadata(metric string) (v1.Metadata, error)
	AllMetadata() (map[string][]v1.Metadata, error)
	LabelNames(metricName string) ([]string, error)
	LabelValues(label string) ([]model.LabelValue, error)
	ChangeDataSource(prometheusURL string) error
	// GetURL is returning the url used to contact the prometheus server
	// In case the instance is used directly in Prometheus, it should be the externalURL
	GetURL() string
}

// httpClient is an implementation of the interface Client.
// You should use this instance directly and not the other one (compatibleHTTPClient and notCompatibleHTTPClient)
// because it will manage which sub instance of the Client to use (like a factory)
type httpClient struct {
	Client
	requestTimeout time.Duration
	mutex          sync.RWMutex
	subClient      Client
	url            string
}

func NewClient(prometheusURL string) (Client, error) {
	c := &httpClient{
		requestTimeout: 30 * time.Second,
	}
	if err := c.ChangeDataSource(prometheusURL); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *httpClient) Metadata(metric string) (v1.Metadata, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.subClient.Metadata(metric)
}

func (c *httpClient) AllMetadata() (map[string][]v1.Metadata, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.subClient.AllMetadata()
}

func (c *httpClient) LabelNames(name string) ([]string, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.subClient.LabelNames(name)
}

func (c *httpClient) LabelValues(label string) ([]model.LabelValue, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.subClient.LabelValues(label)
}

func (c *httpClient) GetURL() string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.url
}

func (c *httpClient) ChangeDataSource(prometheusURL string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.url = prometheusURL
	if len(prometheusURL) == 0 {
		// having an empty URL is a valid use case. So we should just initialized a "fake" http client
		c.subClient = &emptyHTTPClient{}
		return nil
	}
	prometheusHTTPClient, err := api.NewClient(api.Config{
		RoundTripper: buildGenericRoundTripper(c.requestTimeout * time.Second),
		Address:      prometheusURL,
	})
	if err != nil {
		// always initialized the sub client to avoid any nil pointer usage
		if c.subClient == nil {
			c.subClient = &emptyHTTPClient{}
		}
		return err
	}

	isCompatible, err := c.isCompatible(prometheusURL)
	if err != nil {
		// always initialized the sub client to avoid any nil pointer usage
		if c.subClient == nil {
			c.subClient = &emptyHTTPClient{}
		}
		return err
	}
	if isCompatible {
		c.subClient = &compatibleHTTPClient{
			requestTimeout:   c.requestTimeout,
			prometheusClient: v1.NewAPI(prometheusHTTPClient),
		}
	} else {
		c.subClient = &notCompatibleHTTPClient{
			requestTimeout:   c.requestTimeout,
			prometheusClient: v1.NewAPI(prometheusHTTPClient),
		}
	}

	return nil
}

func (c *httpClient) isCompatible(prometheusURL string) (bool, error) {
	httpRequest, err := buildStatusRequest(prometheusURL)
	if err != nil {
		return false, err
	}
	httpClient := &http.Client{
		Transport: buildGenericRoundTripper(c.requestTimeout * time.Second),
		Timeout:   c.requestTimeout * time.Second,
	}
	resp, err := httpClient.Do(httpRequest)
	if err != nil {
		return false, err
	}

	// For prometheus version less than 2.14 `api/v1/status/buildinfo` was not supported this can
	// break many function which solely depends on version comparing like `hover`, etc.
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	defer resp.Body.Close() // nolint: errcheck
	if resp.Body != nil {
		data, err := ioutil.ReadAll(resp.Body)
		jsonResponse := buildInfoResponse{}
		err = json.Unmarshal(data, &jsonResponse)
		if err != nil {
			return false, err
		}
		currentVersion, err := semver.New(jsonResponse.Data.Version)
		if err != nil {
			return false, err
		}
		return currentVersion.GTE(requiredVersion), nil
	}
	return false, nil
}
