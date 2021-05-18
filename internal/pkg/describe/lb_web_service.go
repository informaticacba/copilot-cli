// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package describe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation/stack"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
)

const (
	envOutputPublicLoadBalancerDNSName = "PublicLoadBalancerDNSName"
	envOutputSubdomain                 = "EnvironmentSubdomain"
)

// LBWebServiceURI represents the unique identifier to access a web service.
type LBWebServiceURI struct {
	DNSName string // The environment's subdomain if the service is served on HTTPS. Otherwise, the public load balancer's DNS.
	Path    string // Empty if the service is served on HTTPS. Otherwise, the pattern used to match the service.
}

func (uri *LBWebServiceURI) String() string {
	switch uri.Path {
	// When the service is using host based routing, the service
	// is included in the DNS name (svc.myenv.myproj.dns.com)
	case "":
		return fmt.Sprintf("https://%s", uri.DNSName)
	// When the service is using the root path, there is no "path"
	// (for example http://lb.us-west-2.amazon.com/)
	case "/":
		return fmt.Sprintf("http://%s", uri.DNSName)
	// Otherwise, if there is a path for the service, link to the
	// LoadBalancer DNS name and the path
	// (for example http://lb.us-west-2.amazon.com/svc)
	default:
		return fmt.Sprintf("http://%s/%s", uri.DNSName, uri.Path)
	}
}

type serviceDiscovery struct {
	Service string
	App     string
	Port    string
}

func (s *serviceDiscovery) String() string {
	return fmt.Sprintf("%s.%s.local:%s", s.Service, s.App, s.Port)
}

// LBWebServiceDescriber retrieves information about a load balanced web service.
type LBWebServiceDescriber struct {
	app             string
	svc             string
	enableResources bool

	store                DeployedEnvServicesLister
	envSvcDescribers     map[string]ecsSvcDescriber
	initServiceDescriber func(string) error

	// cache only last svc paramerters
	svcParams map[string]string
}

// NewLBWebServiceConfig contains fields that initiates WebServiceDescriber struct.
type NewLBWebServiceConfig struct {
	NewServiceConfig
	EnableResources bool
	DeployStore     DeployedEnvServicesLister
}

// NewLBWebServiceDescriber instantiates a load balanced service describer.
func NewLBWebServiceDescriber(opt NewLBWebServiceConfig) (*LBWebServiceDescriber, error) {
	describer := &LBWebServiceDescriber{
		app:              opt.App,
		svc:              opt.Svc,
		enableResources:  opt.EnableResources,
		store:            opt.DeployStore,
		envSvcDescribers: make(map[string]ecsSvcDescriber),
	}
	describer.initServiceDescriber = func(env string) error {
		if _, ok := describer.envSvcDescribers[env]; ok {
			return nil
		}
		d, err := NewECSServiceDescriber(NewServiceConfig{
			App:         opt.App,
			Env:         env,
			Svc:         opt.Svc,
			ConfigStore: opt.ConfigStore,
		})
		if err != nil {
			return err
		}
		describer.envSvcDescribers[env] = d
		return nil
	}
	return describer, nil
}

// Describe returns info of a web service.
func (d *LBWebServiceDescriber) Describe() (HumanJSONStringer, error) {
	environments, err := d.store.ListEnvironmentsDeployedTo(d.app, d.svc)
	if err != nil {
		return nil, fmt.Errorf("list deployed environments for application %s: %w", d.app, err)
	}

	var routes []*WebServiceRoute
	var configs []*ECSServiceConfig
	var serviceDiscoveries []*ServiceDiscovery
	var envVars []*containerEnvVar
	var secrets []*secret
	for _, env := range environments {
		err := d.initServiceDescriber(env)
		if err != nil {
			return nil, err
		}
		webServiceURI, err := d.URI(env)
		if err != nil {
			return nil, fmt.Errorf("retrieve service URI: %w", err)
		}
		routes = append(routes, &WebServiceRoute{
			Environment: env,
			URL:         webServiceURI,
		})
		configs = append(configs, &ECSServiceConfig{
			ServiceConfig: &ServiceConfig{
				Environment: env,
				Port:        d.svcParams[stack.LBWebServiceContainerPortParamKey],
				CPU:         d.svcParams[stack.WorkloadTaskCPUParamKey],
				Memory:      d.svcParams[stack.WorkloadTaskMemoryParamKey],
			},
			Tasks: d.svcParams[stack.WorkloadTaskCountParamKey],
		})
		serviceDiscoveries = appendServiceDiscovery(serviceDiscoveries, serviceDiscovery{
			Service: d.svc,
			Port:    d.svcParams[stack.LBWebServiceContainerPortParamKey],
			App:     d.app,
		}, env)
		webSvcEnvVars, err := d.envSvcDescribers[env].EnvVars()
		if err != nil {
			return nil, fmt.Errorf("retrieve environment variables: %w", err)
		}
		envVars = append(envVars, flattenContainerEnvVars(env, webSvcEnvVars)...)
		webSvcSecrets, err := d.envSvcDescribers[env].Secrets()
		if err != nil {
			return nil, fmt.Errorf("retrieve secrets: %w", err)
		}
		secrets = append(secrets, flattenSecrets(env, webSvcSecrets)...)
	}
	resources := make(map[string][]*CfnResource)
	if d.enableResources {
		for _, env := range environments {
			err := d.initServiceDescriber(env)
			if err != nil {
				return nil, err
			}
			stackResources, err := d.envSvcDescribers[env].ServiceStackResources()
			if err != nil {
				return nil, fmt.Errorf("retrieve service resources: %w", err)
			}
			resources[env] = flattenResources(stackResources)
		}
	}

	return &webSvcDesc{
		Service:          d.svc,
		Type:             manifest.LoadBalancedWebServiceType,
		App:              d.app,
		Configurations:   configs,
		Routes:           routes,
		ServiceDiscovery: serviceDiscoveries,
		Variables:        envVars,
		Secrets:          secrets,
		Resources:        resources,

		environments: environments,
	}, nil
}

// URI returns the LBWebServiceURI to identify this service uniquely given an environment name.
func (d *LBWebServiceDescriber) URI(envName string) (string, error) {
	err := d.initServiceDescriber(envName)
	if err != nil {
		return "", err
	}

	envOutputs, err := d.envSvcDescribers[envName].EnvOutputs()
	if err != nil {
		return "", fmt.Errorf("get output for environment %s: %w", envName, err)
	}
	svcParams, err := d.envSvcDescribers[envName].Params()
	if err != nil {
		return "", fmt.Errorf("get parameters for service %s: %w", d.svc, err)
	}
	d.svcParams = svcParams

	uri := &LBWebServiceURI{
		DNSName: envOutputs[envOutputPublicLoadBalancerDNSName],
		Path:    svcParams[stack.LBWebServiceRulePathParamKey],
	}
	_, isHTTPS := envOutputs[envOutputSubdomain]
	if isHTTPS {
		dnsName := fmt.Sprintf("%s.%s", d.svc, envOutputs[envOutputSubdomain])
		uri = &LBWebServiceURI{
			DNSName: dnsName,
		}
	}
	return uri.String(), nil
}

type secret struct {
	Name        string `json:"name"`
	Container   string `json:"container"`
	Environment string `json:"environment"`
	ValueFrom   string `json:"valueFrom"`
}

type secrets []*secret

func (s secrets) humanString(w io.Writer) {
	headers := []string{"Name", "Container", "Environment", "Value From"}
	fmt.Fprintf(w, "  %s\n", strings.Join(headers, "\t"))
	fmt.Fprintf(w, "  %s\n", strings.Join(underline(headers), "\t"))
	sort.SliceStable(s, func(i, j int) bool { return s[i].Environment < s[j].Environment })
	sort.SliceStable(s, func(i, j int) bool { return s[i].Container < s[j].Container })
	sort.SliceStable(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	if len(s) > 0 {
		valueFrom := s[0].ValueFrom
		if _, err := arn.Parse(s[0].ValueFrom); err != nil {
			// If the valueFrom is not an ARN, preface it with "parameter/"
			valueFrom = fmt.Sprintf("parameter/%s", s[0].ValueFrom)
		}
		fmt.Fprintf(w, "  %s\n", strings.Join([]string{s[0].Name, s[0].Container, s[0].Environment, valueFrom}, "\t"))
	}
	for prev, cur := 0, 1; cur < len(s); prev, cur = prev+1, cur+1 {
		valueFrom := s[cur].ValueFrom
		if _, err := arn.Parse(s[cur].ValueFrom); err != nil {
			// If the valueFrom is not an ARN, preface it with "parameter/"
			valueFrom = fmt.Sprintf("parameter/%s", s[cur].ValueFrom)
		}
		cols := []string{s[cur].Name, s[cur].Container, s[cur].Environment, valueFrom}
		if s[prev].Name == s[cur].Name {
			cols[0] = dittoSymbol
		}
		if s[prev].Container == s[cur].Container {
			cols[1] = dittoSymbol
		}
		if s[prev].Environment == s[cur].Environment {
			cols[2] = dittoSymbol
		}
		if s[prev].ValueFrom == s[cur].ValueFrom {
			cols[3] = dittoSymbol
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(cols, "\t"))
	}
}

func underline(headings []string) []string {
	var lines []string
	for _, heading := range headings {
		line := strings.Repeat("-", len(heading))
		lines = append(lines, line)
	}
	return lines
}

// WebServiceRoute contains serialized route parameters for a web service.
type WebServiceRoute struct {
	Environment string `json:"environment"`
	URL         string `json:"url"`
}

// ServiceDiscovery contains serialized service discovery info for an service.
type ServiceDiscovery struct {
	Environment []string `json:"environment"`
	Namespace   string   `json:"namespace"`
}

type serviceDiscoveries []*ServiceDiscovery

func (s serviceDiscoveries) humanString(w io.Writer) {
	headers := []string{"Environment", "Namespace"}
	fmt.Fprintf(w, "  %s\n", strings.Join(headers, "\t"))
	fmt.Fprintf(w, "  %s\n", strings.Join(underline(headers), "\t"))
	for _, sd := range s {
		fmt.Fprintf(w, "  %s\t%s\n", strings.Join(sd.Environment, ", "), sd.Namespace)
	}
}

// webSvcDesc contains serialized parameters for a web service.
type webSvcDesc struct {
	Service          string             `json:"service"`
	Type             string             `json:"type"`
	App              string             `json:"application"`
	Configurations   ecsConfigurations  `json:"configurations"`
	Routes           []*WebServiceRoute `json:"routes"`
	ServiceDiscovery serviceDiscoveries `json:"serviceDiscovery"`
	Variables        containerEnvVars   `json:"variables"`
	Secrets          secrets            `json:"secrets,omitempty"`
	Resources        cfnResources       `json:"resources,omitempty"`

	environments []string
}

// JSONString returns the stringified webSvcDesc struct in json format.
func (w *webSvcDesc) JSONString() (string, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("marshal web service description: %w", err)
	}
	return fmt.Sprintf("%s\n", b), nil
}

// HumanString returns the stringified webService struct in human readable format.
func (w *webSvcDesc) HumanString() string {
	var b bytes.Buffer
	writer := tabwriter.NewWriter(&b, minCellWidth, tabWidth, cellPaddingWidth, paddingChar, noAdditionalFormatting)
	fmt.Fprint(writer, color.Bold.Sprint("About\n\n"))
	writer.Flush()
	fmt.Fprintf(writer, "  %s\t%s\n", "Application", w.App)
	fmt.Fprintf(writer, "  %s\t%s\n", "Name", w.Service)
	fmt.Fprintf(writer, "  %s\t%s\n", "Type", w.Type)
	fmt.Fprint(writer, color.Bold.Sprint("\nConfigurations\n\n"))
	writer.Flush()
	w.Configurations.humanString(writer)
	fmt.Fprint(writer, color.Bold.Sprint("\nRoutes\n\n"))
	writer.Flush()
	headers := []string{"Environment", "URL"}
	fmt.Fprintf(writer, "  %s\n", strings.Join(headers, "\t"))
	fmt.Fprintf(writer, "  %s\n", strings.Join(underline(headers), "\t"))
	for _, route := range w.Routes {
		fmt.Fprintf(writer, "  %s\t%s\n", route.Environment, route.URL)
	}
	fmt.Fprint(writer, color.Bold.Sprint("\nService Discovery\n\n"))
	writer.Flush()
	w.ServiceDiscovery.humanString(writer)
	fmt.Fprint(writer, color.Bold.Sprint("\nVariables\n\n"))
	writer.Flush()
	w.Variables.humanString(writer)
	if len(w.Secrets) != 0 {
		fmt.Fprint(writer, color.Bold.Sprint("\nSecrets\n\n"))
		writer.Flush()
		w.Secrets.humanString(writer)
	}
	if len(w.Resources) != 0 {
		fmt.Fprint(writer, color.Bold.Sprint("\nResources\n"))
		writer.Flush()

		w.Resources.humanStringByEnv(writer, w.environments)
	}
	writer.Flush()
	return b.String()
}

func cpuToString(s string) string {
	cpuInt, _ := strconv.Atoi(s)
	cpuFloat := float64(cpuInt) / 1024
	return fmt.Sprintf("%g", cpuFloat)
}

// IsStackNotExistsErr returns true if error type is stack not exist.
func IsStackNotExistsErr(err error) bool {
	if err == nil {
		return false
	}
	aerr, ok := err.(awserr.Error)
	if !ok {
		return IsStackNotExistsErr(errors.Unwrap(err))
	}
	if aerr.Code() != "ValidationError" {
		return IsStackNotExistsErr(errors.Unwrap(err))
	}
	if !strings.Contains(aerr.Message(), "does not exist") {
		return IsStackNotExistsErr(errors.Unwrap(err))
	}
	return true
}

func appendServiceDiscovery(sds []*ServiceDiscovery, sd serviceDiscovery, env string) []*ServiceDiscovery {
	exist := false
	for _, s := range sds {
		if s.Namespace == sd.String() {
			s.Environment = append(s.Environment, env)
			exist = true
		}
	}
	if !exist {
		sds = append(sds, &ServiceDiscovery{
			Environment: []string{env},
			Namespace:   sd.String(),
		})
	}
	return sds
}
