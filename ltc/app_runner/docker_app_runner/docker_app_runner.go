package docker_app_runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cloudfoundry-incubator/lattice/ltc/app_runner/docker_repository_name_formatter"
	"github.com/cloudfoundry-incubator/lattice/ltc/logs/reserved_app_ids"
	"github.com/cloudfoundry-incubator/lattice/ltc/route_helpers"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

type MonitorMethod int

const (
	NoMonitor MonitorMethod = iota
	PortMonitor
	URLMonitor

	AttemptedToCreateLatticeDebugErrorMessage = reserved_app_ids.LatticeDebugLogStreamAppId + " is a reserved app name. It is used internally to stream debug logs for lattice components."
)

//go:generate counterfeiter -o fake_app_runner/fake_app_runner.go . AppRunner
type AppRunner interface {
	CreateDockerApp(params CreateDockerAppParams) error
	SubmitLrp(submitLrpJson []byte) (string, error)
	ScaleApp(name string, instances int) error
	UpdateAppRoutes(name string, routes RouteOverrides) error
	RemoveApp(name string) error
}

type MonitorConfig struct {
	Method  MonitorMethod
	URI     string
	Port    uint16
	Timeout time.Duration
}

type RouteOverrides []RouteOverride

type RouteOverride struct {
	HostnamePrefix string
	Port           uint16
}

type CreateDockerAppParams struct {
	Name                 string
	StartCommand         string
	DockerImagePath      string
	AppArgs              []string
	EnvironmentVariables map[string]string
	Privileged           bool
	Monitor              MonitorConfig
	Instances            int
	CPUWeight            uint
	MemoryMB             int
	DiskMB               int
	ExposedPorts         []uint16
	WorkingDir           string
	RouteOverrides       RouteOverrides
	NoRoutes             bool
	Timeout              time.Duration
}

const (
	healthcheckDownloadUrl string = "http://file_server.service.dc1.consul:8080/v1/static/healthcheck.tgz"
	lrpDomain              string = "lattice"
)

type appRunner struct {
	receptorClient receptor.Client
	systemDomain   string
}

func New(receptorClient receptor.Client, systemDomain string) AppRunner {
	return &appRunner{receptorClient, systemDomain}
}

func (appRunner *appRunner) CreateDockerApp(params CreateDockerAppParams) error {
	if params.Name == reserved_app_ids.LatticeDebugLogStreamAppId {
		return errors.New(AttemptedToCreateLatticeDebugErrorMessage)
	}
	if exists, err := appRunner.desiredLRPExists(params.Name); err != nil {
		return err
	} else if exists {
		return newExistingAppError(params.Name)
	}

	if err := appRunner.receptorClient.UpsertDomain(lrpDomain, 0); err != nil {
		return err
	}

	return appRunner.desireLrp(params)
}

func (appRunner *appRunner) SubmitLrp(submitLrpJson []byte) (string, error) {

	desiredLRP := receptor.DesiredLRPCreateRequest{}

	err := json.Unmarshal(submitLrpJson, &desiredLRP)
	if err != nil {
		return "", err
	}

	if desiredLRP.ProcessGuid == reserved_app_ids.LatticeDebugLogStreamAppId {
		return desiredLRP.ProcessGuid, errors.New(AttemptedToCreateLatticeDebugErrorMessage)
	}

	if exists, err := appRunner.desiredLRPExists(desiredLRP.ProcessGuid); err != nil {
		return desiredLRP.ProcessGuid, err
	} else if exists {
		return desiredLRP.ProcessGuid, newExistingAppError(desiredLRP.ProcessGuid)
	}

	if err := appRunner.receptorClient.UpsertDomain(lrpDomain, 0); err != nil {
		return desiredLRP.ProcessGuid, err
	}

	err = appRunner.receptorClient.CreateDesiredLRP(desiredLRP)
	return desiredLRP.ProcessGuid, err
}

func (appRunner *appRunner) ScaleApp(name string, instances int) error {
	if exists, err := appRunner.desiredLRPExists(name); err != nil {
		return err
	} else if !exists {
		return newAppNotStartedError(name)
	}

	return appRunner.updateLrpInstances(name, instances)
}

func (appRunner *appRunner) UpdateAppRoutes(name string, routes RouteOverrides) error {
	if exists, err := appRunner.desiredLRPExists(name); err != nil {
		return err
	} else if !exists {
		return newAppNotStartedError(name)
	}

	return appRunner.updateLrpRoutes(name, routes)
}

func (appRunner *appRunner) RemoveApp(name string) error {
	if lrpExists, err := appRunner.desiredLRPExists(name); err != nil {
		return err
	} else if !lrpExists {
		return newAppNotStartedError(name)
	}

	return appRunner.receptorClient.DeleteDesiredLRP(name)
}

func (appRunner *appRunner) desiredLRPExists(name string) (exists bool, err error) {
	desiredLRPs, err := appRunner.receptorClient.DesiredLRPs()
	if err != nil {
		return false, err
	}

	for _, desiredLRP := range desiredLRPs {
		if desiredLRP.ProcessGuid == name {
			return true, nil
		}
	}

	return false, nil
}

func (appRunner *appRunner) desireLrp(params CreateDockerAppParams) error {
	dockerImageUrl, err := docker_repository_name_formatter.FormatForReceptor(params.DockerImagePath)
	if err != nil {
		return err
	}

	envVars := buildEnvironmentVariables(params.EnvironmentVariables)
	envVars = append(envVars, receptor.EnvironmentVariable{Name: "PORT", Value: fmt.Sprintf("%d", params.Monitor.Port)})

	var appRoutes route_helpers.AppRoutes
	if params.NoRoutes {
		appRoutes = route_helpers.AppRoutes{}
	} else if len(params.RouteOverrides) > 0 {
		routeMap := make(map[uint16][]string)
		for _, override := range params.RouteOverrides {
			routeMap[override.Port] = append(routeMap[override.Port], fmt.Sprintf("%s.%s", override.HostnamePrefix, appRunner.systemDomain))
		}
		for port, hostnames := range routeMap {
			appRoutes = append(appRoutes, route_helpers.AppRoute{
				Hostnames: hostnames,
				Port:      port,
			})
		}
	} else {
		appRoutes = appRunner.buildDefaultRoutingInfo(params.Name, params.ExposedPorts, params.Monitor.Port)
	}

	req := receptor.DesiredLRPCreateRequest{
		ProcessGuid:          params.Name,
		Domain:               lrpDomain,
		RootFS:               dockerImageUrl,
		Instances:            params.Instances,
		Routes:               appRoutes.RoutingInfo(),
		CPUWeight:            params.CPUWeight,
		MemoryMB:             params.MemoryMB,
		DiskMB:               params.DiskMB,
		Privileged:           true,
		Ports:                params.ExposedPorts,
		LogGuid:              params.Name,
		LogSource:            "APP",
		MetricsGuid:          params.Name,
		EnvironmentVariables: envVars,
		Setup: &models.DownloadAction{
			From: healthcheckDownloadUrl,
			To:   "/tmp",
		},
		Action: &models.RunAction{
			Path:       params.StartCommand,
			Args:       params.AppArgs,
			Privileged: params.Privileged,
			Dir:        params.WorkingDir,
		},
	}

	var healthCheckArgs []string
	if params.Monitor.Timeout != 0 {
		healthCheckArgs = append(healthCheckArgs, "-timeout", fmt.Sprint(params.Monitor.Timeout))
	}
	switch params.Monitor.Method {
	case PortMonitor:
		req.Monitor = &models.RunAction{
			Path:      "/tmp/healthcheck",
			Args:      append(healthCheckArgs, "-port", fmt.Sprint(params.Monitor.Port)),
			LogSource: "HEALTH",
		}
	case URLMonitor:
		req.Monitor = &models.RunAction{
			Path:      "/tmp/healthcheck",
			Args:      append(healthCheckArgs, "-port", fmt.Sprint(params.Monitor.Port), "-uri", params.Monitor.URI),
			LogSource: "HEALTH",
		}
	}

	return appRunner.receptorClient.CreateDesiredLRP(req)
}

func (appRunner *appRunner) updateLrpInstances(name string, instances int) error {
	err := appRunner.receptorClient.UpdateDesiredLRP(
		name,
		receptor.DesiredLRPUpdateRequest{
			Instances: &instances,
		},
	)

	return err
}

func (appRunner *appRunner) updateLrpRoutes(name string, routes RouteOverrides) error {
	appRoutes := route_helpers.AppRoutes{}

	routeMap := make(map[uint16][]string)
	for _, override := range routes {
		routeMap[override.Port] = append(routeMap[override.Port], fmt.Sprintf("%s.%s", override.HostnamePrefix, appRunner.systemDomain))
	}
	for port, hostnames := range routeMap {
		appRoutes = append(appRoutes, route_helpers.AppRoute{
			Hostnames: hostnames,
			Port:      port,
		})
	}

	err := appRunner.receptorClient.UpdateDesiredLRP(
		name,
		receptor.DesiredLRPUpdateRequest{
			Routes: appRoutes.RoutingInfo(),
		},
	)

	return err
}

func (appRunner *appRunner) buildDefaultRoutingInfo(appName string, exposedPorts []uint16, monitorPort uint16) route_helpers.AppRoutes {
	appRoutes := route_helpers.AppRoutes{}

	for _, port := range exposedPorts {
		hostnames := []string{}
		if port == monitorPort {
			hostnames = append(hostnames, fmt.Sprintf("%s.%s", appName, appRunner.systemDomain))
		}

		hostnames = append(hostnames, fmt.Sprintf("%s-%s.%s", appName, strconv.Itoa(int(port)), appRunner.systemDomain))
		appRoutes = append(appRoutes, route_helpers.AppRoute{
			Hostnames: hostnames,
			Port:      port,
		})
	}

	return appRoutes
}

func buildEnvironmentVariables(environmentVariables map[string]string) []receptor.EnvironmentVariable {
	appEnvVars := make([]receptor.EnvironmentVariable, 0, len(environmentVariables)+1)
	for name, value := range environmentVariables {
		appEnvVars = append(appEnvVars, receptor.EnvironmentVariable{Name: name, Value: value})
	}
	return appEnvVars
}
