//go:build !linux

package main

import "errors"

func edgeScheduleAgentUninstall(controller, statePath, nodeGUID, requestID string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}

func launchAgentUninstallHelper(controller, statePath, nodeGUID, requestID string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}

func runAgentUninstallHelper(args []string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}
