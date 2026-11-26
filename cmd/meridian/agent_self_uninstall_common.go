package main

import (
	"errors"
	"regexp"
	"strings"
)

const (
	agentManagedStatePath      = "/var/lib/meridian-agent/state.json"
	agentUninstallMarkerSuffix = ".uninstalling"
)

type agentUninstallRecoveryPaths struct {
	ServiceName string
	Binary      string
	Unit        string
	Init        string
	Done        string
	Lock        string
}

var agentUninstallRequestIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func agentSelfUninstallRequestMatches(config AgentRuntimeConfig, nodeGUID string) bool {
	return config.SchemaVersion == agentConfigSchemaVersion &&
		strings.TrimSpace(config.NodeGUID) != "" &&
		config.NodeGUID == nodeGUID &&
		agentUninstallRequestIDPattern.MatchString(config.UninstallRequestID)
}

type agentUninstallCompletion struct {
	NodeGUID  string `json:"node_guid"`
	RequestID string `json:"request_id"`
}

func validAgentUninstallCompletion(completion agentUninstallCompletion) bool {
	return validTLSNodeGUID(strings.TrimSpace(completion.NodeGUID)) &&
		agentUninstallRequestIDPattern.MatchString(completion.RequestID)
}

func agentUninstallRecoveryPathsFor(requestID string) (agentUninstallRecoveryPaths, error) {
	if !agentUninstallRequestIDPattern.MatchString(requestID) {
		return agentUninstallRecoveryPaths{}, errors.New("invalid Agent uninstall recovery request ID")
	}
	serviceName := "meridian-agent-uninstall-" + requestID
	return agentUninstallRecoveryPaths{
		ServiceName: serviceName,
		Binary:      "/usr/local/libexec/" + serviceName,
		Unit:        "/etc/systemd/system/" + serviceName + ".service",
		Init:        "/etc/init.d/" + serviceName,
		Done:        "/etc/" + serviceName + ".done",
		Lock:        "/run/" + serviceName + ".lock",
	}, nil
}

func agentUninstallRecoveryMayRemoveState(completion agentUninstallCompletion, state edgeAgentState) bool {
	return validAgentUninstallCompletion(completion) && state.Token != "" && state.NodeGUID == completion.NodeGUID
}
