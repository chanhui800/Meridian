//go:build !linux

package main

import "errors"

func edgeScheduleAgentUninstall(controller, nodeGUID, requestID string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}

func launchAgentUninstallHelper(controller, nodeGUID, requestID string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}

func runAgentUninstallHelper(args []string) error {
	return errors.New("Agent self-uninstall is supported only on Linux")
}
