//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	agentUninstallTaskSuffix = ".uninstall.json"
)

type agentUninstallTask struct {
	Controller string `json:"controller"`
	NodeGUID   string `json:"node_guid"`
	RequestID  string `json:"request_id"`
}

func edgeScheduleAgentUninstall(controller, nodeGUID, requestID string) error {
	controller, err := edgeNormalizeController(controller)
	if err != nil {
		return err
	}
	if !validTLSNodeGUID(nodeGUID) || !agentUninstallRequestIDPattern.MatchString(requestID) {
		return errors.New("Agent uninstall request has an invalid identity")
	}
	state, err := edgeLoadState(agentManagedStatePath)
	if err != nil || state.Token == "" || state.NodeGUID != nodeGUID {
		return errors.New("Agent uninstall request does not match the enrolled node")
	}
	marker := agentManagedStatePath + agentUninstallMarkerSuffix
	if data, readErr := os.ReadFile(marker); readErr == nil {
		if strings.TrimSpace(string(data)) != requestID {
			return errors.New("a different Agent uninstall request is already pending")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else if err := writeAgentUninstallFile(marker, []byte(requestID+"\n"), 0o600); err != nil {
		return fmt.Errorf("persist Agent uninstall marker: %w", err)
	}
	if err := writeAgentUninstallTask(agentUninstallTask{Controller: controller, NodeGUID: nodeGUID, RequestID: requestID}); err != nil {
		return fmt.Errorf("persist Agent uninstall recovery task: %w", err)
	}
	return launchAgentUninstallHelper(controller, nodeGUID, requestID)
}

func launchAgentUninstallHelper(controller, nodeGUID, requestID string) error {
	paths, err := agentUninstallRecoveryPathsFor(requestID)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--agent-uninstall-helper", "--controller", controller, "--state", agentManagedStatePath, "--node-guid", nodeGUID, "--request-id", requestID}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		unit := paths.ServiceName + "-bootstrap"
		command := exec.Command("systemd-run", append([]string{"--quiet", "--no-block", "--collect", "--unit=" + unit, "--property=Type=oneshot"}, append([]string{executable}, args...)...)...) // #nosec G204 -- executable is the running Agent binary; request IDs/GUIDs are validated and args are passed without a shell.
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("could not start the systemd Agent uninstall helper: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	if _, err := os.Stat("/sbin/openrc-run"); err == nil {
		pidFile := "/run/" + paths.ServiceName + "-bootstrap.pid"
		command := exec.Command("start-stop-daemon", append([]string{"--start", "--background", "--make-pidfile", "--pidfile", pidFile, "--exec", executable, "--"}, args...)...) // #nosec G204 -- executable is the running Agent binary; request IDs/GUIDs are validated and args are passed without a shell.
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("could not start the OpenRC Agent uninstall helper: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	return errors.New("Agent uninstall is supported only with systemd or OpenRC")
}

func runAgentUninstallHelper(args []string) error {
	flags := flag.NewFlagSet("agent-uninstall-helper", flag.ContinueOnError)
	controllerValue := flags.String("controller", "", "Controller HTTPS URL")
	statePathValue := flags.String("state", "/var/lib/meridian-agent/state.json", "Agent state file")
	nodeGUIDValue := flags.String("node-guid", "", "Enrolled node GUID")
	requestIDValue := flags.String("request-id", "", "Controller uninstall request ID")
	recoveryValue := flags.Bool("recovery", false, "run as the persistent uninstall recovery service")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if filepath.Clean(*statePathValue) != agentManagedStatePath {
		return errors.New("refusing an uninstall request outside the managed Agent identity")
	}
	if !*recoveryValue {
		controller, err := edgeNormalizeController(*controllerValue)
		if err != nil {
			return err
		}
		if !validTLSNodeGUID(*nodeGUIDValue) || !agentUninstallRequestIDPattern.MatchString(*requestIDValue) {
			return errors.New("refusing an uninstall request with an invalid node identity")
		}
		markerData, err := os.ReadFile(agentManagedStatePath + agentUninstallMarkerSuffix)
		if err != nil || strings.TrimSpace(string(markerData)) != *requestIDValue {
			return errors.New("Agent uninstall marker does not match the requested operation")
		}
		state, err := edgeLoadState(agentManagedStatePath)
		if err != nil || state.Token == "" || state.NodeGUID != *nodeGUIDValue {
			return errors.New("Agent uninstall helper could not verify its local node identity")
		}
		task := agentUninstallTask{Controller: controller, NodeGUID: *nodeGUIDValue, RequestID: *requestIDValue}
		if err := writeAgentUninstallTask(task); err != nil {
			return err
		}
		return installAgentUninstallRecoveryService(task.RequestID)
	}
	paths, err := agentUninstallRecoveryPathsFor(*requestIDValue)
	if err != nil {
		return err
	}
	unlock, err := acquireAgentInstanceLock(paths.Lock)
	if errors.Is(err, errAgentAlreadyRunning) {
		// A retry or reboot recovery service already owns the uninstall. The
		// service manager will retry this invocation if the lock owner exits.
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	if data, doneErr := os.ReadFile(paths.Done); doneErr == nil {
		var completion agentUninstallCompletion
		if err := json.Unmarshal(data, &completion); err != nil || !validAgentUninstallCompletion(completion) || completion.RequestID != *requestIDValue {
			return errors.New("Agent uninstall completion marker is invalid")
		}
		for attempt := 0; ; attempt++ {
			if err := finishAgentUninstallRecovery(completion); err != nil {
				fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for final file cleanup: %v\n", err)
				sleepAgentUninstallRetry(attempt)
				continue
			}
			return nil
		}
	} else if !errors.Is(doneErr, os.ErrNotExist) {
		return doneErr
	}
	task, err := readAgentUninstallTask()
	if err != nil {
		return err
	}
	if err := validateAgentUninstallTask(task); err != nil {
		return err
	}
	state, err := edgeLoadState(agentManagedStatePath)
	if err != nil || state.Token == "" || state.NodeGUID != task.NodeGUID {
		return errors.New("Agent uninstall recovery could not verify its local node identity")
	}
	if task.RequestID != *requestIDValue {
		return errors.New("Agent uninstall recovery service request ID does not match its task")
	}
	if err := runAgentUninstallToCompletion(task, state.Token); err != nil {
		return err
	}
	return nil
}

func writeAgentUninstallTask(task agentUninstallTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return writeAgentUninstallFile(agentManagedStatePath+agentUninstallTaskSuffix, append(data, '\n'), 0o600)
}

func readAgentUninstallTask() (agentUninstallTask, error) {
	data, err := os.ReadFile(agentManagedStatePath + agentUninstallTaskSuffix)
	if err != nil {
		return agentUninstallTask{}, err
	}
	var task agentUninstallTask
	if err := json.Unmarshal(data, &task); err != nil {
		return agentUninstallTask{}, err
	}
	task.Controller, err = edgeNormalizeController(task.Controller)
	if err != nil || !validTLSNodeGUID(task.NodeGUID) || !agentUninstallRequestIDPattern.MatchString(task.RequestID) {
		return agentUninstallTask{}, errors.New("Agent uninstall recovery task is invalid")
	}
	return task, nil
}

func validateAgentUninstallTask(task agentUninstallTask) error {
	markerData, err := os.ReadFile(agentManagedStatePath + agentUninstallMarkerSuffix)
	if err != nil || strings.TrimSpace(string(markerData)) != task.RequestID {
		return errors.New("Agent uninstall marker does not match the recovery task")
	}
	return nil
}

func writeAgentUninstallFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".meridian-agent-uninstall-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		current, readErr := os.ReadFile(path) // #nosec G304 -- path is generated from the managed Agent state directory or request-scoped recovery paths.
		if readErr != nil {
			return readErr
		}
		if string(current) != string(data) {
			return fmt.Errorf("Agent uninstall recovery path %q already exists with different contents", path)
		}
	}
	return nil
}

func installAgentUninstallRecoveryService(requestID string) error {
	paths, err := agentUninstallRecoveryPathsFor(requestID)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := installAgentUninstallRecoveryBinary(executable, paths.Binary); err != nil {
		return err
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		unit := "[Unit]\nDescription=Meridian Agent uninstall recovery\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=" + paths.Binary + " --agent-uninstall-helper --state /var/lib/meridian-agent/state.json --request-id " + requestID + " --recovery\nRestart=on-failure\nRestartSec=30s\n\n[Install]\nWantedBy=multi-user.target\n"
		if err := writeAgentUninstallFile(paths.Unit, []byte(unit), 0o644); err != nil {
			return err
		}
		if err := runAgentUninstallManagerCommand("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("could not register the Agent uninstall recovery unit: %w", err)
		}
		if err := runAgentUninstallManagerCommand("systemctl", "enable", "--now", paths.ServiceName+".service"); err != nil {
			return fmt.Errorf("could not start the Agent uninstall recovery unit: %w", err)
		}
		return nil
	}
	if _, err := os.Stat("/sbin/openrc-run"); err == nil {
		initScript := "#!/sbin/openrc-run\nname=\"" + paths.ServiceName + "\"\ndescription=\"Meridian Agent uninstall recovery\"\ncommand=\"" + paths.Binary + "\"\ncommand_args=\"--agent-uninstall-helper --state /var/lib/meridian-agent/state.json --request-id " + requestID + " --recovery\"\ncommand_user=\"root\"\ncommand_background=\"yes\"\nsupervisor=\"supervise-daemon\"\nrespawn_delay=30\nrespawn_max=0\npidfile=\"/run/" + paths.ServiceName + ".pid\"\noutput_log=\"/var/log/" + paths.ServiceName + ".log\"\nerror_log=\"/var/log/" + paths.ServiceName + ".log\"\ndepend() {\n\tneed net\n}\n"
		if err := writeAgentUninstallFile(paths.Init, []byte(initScript), 0o755); err != nil {
			return err
		}
		if err := runAgentUninstallManagerCommand("rc-update", "add", paths.ServiceName, "default"); err != nil {
			return fmt.Errorf("could not register the OpenRC Agent uninstall recovery service: %w", err)
		}
		if err := runAgentUninstallManagerCommand("rc-service", paths.ServiceName, "start"); err != nil {
			return fmt.Errorf("could not start the OpenRC Agent uninstall recovery service: %w", err)
		}
		return nil
	}
	return errors.New("Agent uninstall recovery is supported only with systemd or OpenRC")
}

func installAgentUninstallRecoveryBinary(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil { // #nosec G301 -- destination is request-scoped under the standard world-readable /usr/local/libexec directory.
		return err
	}
	input, err := os.Open(source) // #nosec G304 -- source is os.Executable(), selected by the kernel.
	if err != nil {
		return err
	}
	defer input.Close()
	file, err := os.CreateTemp(filepath.Dir(destination), ".meridian-agent-uninstall-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := io.Copy(file, input); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o755); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		sourceHash, hashErr := hashAgentUninstallFile(source)
		if hashErr != nil {
			return hashErr
		}
		destinationHash, hashErr := hashAgentUninstallFile(destination)
		if hashErr != nil || sourceHash != destinationHash {
			return errors.New("Agent uninstall recovery binary path is already owned by different contents")
		}
	}
	return nil
}

func hashAgentUninstallFile(path string) ([32]byte, error) {
	var empty [32]byte
	file, err := os.Open(path) // #nosec G304 -- callers pass either the current executable or a request-scoped recovery binary path.
	if err != nil {
		return empty, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return empty, err
	}
	copy(empty[:], hash.Sum(nil))
	return empty, nil
}

func runAgentUninstallToCompletion(task agentUninstallTask, token string) error {
	paths, err := agentUninstallRecoveryPathsFor(task.RequestID)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 12 * time.Second, Transport: edgeHTTPTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for attempt := 0; ; attempt++ {
		if err := stopAndRemoveAgentService(); err != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for service cleanup: %v\n", err)
			sleepAgentUninstallRetry(attempt)
			continue
		}
		if err := os.RemoveAll("/etc/meridian-agent"); err != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for token-file cleanup: %v\n", err)
			sleepAgentUninstallRetry(attempt)
			continue
		}
		if err := os.RemoveAll("/opt/meridian-agent"); err != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for binary cleanup: %v\n", err)
			sleepAgentUninstallRetry(attempt)
			continue
		}
		ackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := edgeAPIRequest(ackCtx, client, http.MethodPost, task.Controller+"/api/agent/uninstall/complete", token, map[string]string{
			"node_guid": task.NodeGUID, "request_id": task.RequestID,
		}, nil)
		cancel()
		if err == nil {
			break
		}
		var apiErr *edgeAPIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			// An administrator may have removed the pending record after this
			// helper had already completed local service cleanup.
			break
		}
		fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for Controller confirmation: %v\n", err)
		sleepAgentUninstallRetry(attempt)
	}
	completion := agentUninstallCompletion{NodeGUID: task.NodeGUID, RequestID: task.RequestID}
	completionData, err := json.Marshal(completion)
	if err != nil {
		return fmt.Errorf("encode Agent uninstall completion state: %w", err)
	}
	if err := writeAgentUninstallFile(paths.Done, append(completionData, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist Agent uninstall completion state: %w", err)
	}
	for attempt := 0; ; attempt++ {
		if err := finishAgentUninstallRecovery(completion); err != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent uninstall is waiting for final file cleanup: %v\n", err)
			sleepAgentUninstallRetry(attempt)
			continue
		}
		return nil
	}
}

func stopAndRemoveAgentService() error {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		if _, err := os.Stat("/etc/systemd/system/meridian-agent.service"); err == nil {
			if err := runAgentUninstallManagerCommand("systemctl", "disable", "--now", "meridian-agent.service"); err != nil {
				return fmt.Errorf("could not stop and disable the Agent service: %w", err)
			}
			if err := os.Remove("/etc/systemd/system/meridian-agent.service"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := runAgentUninstallManagerCommand("systemctl", "daemon-reload"); err != nil {
				return fmt.Errorf("could not reload systemd after removing the Agent unit: %w", err)
			}
		}
		return nil
	}
	if _, err := os.Stat("/sbin/openrc-run"); err == nil {
		if _, err := os.Stat("/etc/init.d/meridian-agent"); err == nil {
			if output, err := exec.Command("rc-update", "del", "meridian-agent", "default").CombinedOutput(); err != nil {
				listing, listingErr := exec.Command("rc-update", "show", "default").CombinedOutput()
				if listingErr != nil || containsOpenRCService(string(listing), "meridian-agent") {
					return fmt.Errorf("could not disable the OpenRC Agent service: %w: %s", err, strings.TrimSpace(string(output)))
				}
			}
			if err := runAgentUninstallManagerCommand("rc-service", "meridian-agent", "stop"); err != nil {
				return fmt.Errorf("could not stop the OpenRC Agent service: %w", err)
			}
			if err := os.Remove("/etc/init.d/meridian-agent"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	return errors.New("Agent uninstall is supported only with systemd or OpenRC")
}

func containsOpenRCService(listing, service string) bool {
	for _, field := range strings.Fields(listing) {
		if strings.TrimSpace(field) == service {
			return true
		}
	}
	return false
}

func finishAgentUninstallRecovery(completion agentUninstallCompletion) error {
	state, stateErr := edgeLoadState(agentManagedStatePath)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return fmt.Errorf("cannot verify Agent identity before final uninstall cleanup: %w", stateErr)
	}
	removeAgentState := stateErr == nil && agentUninstallRecoveryMayRemoveState(completion, state)
	if removeAgentState {
		if err := os.RemoveAll(filepath.Dir(agentManagedStatePath)); err != nil {
			return err
		}
	} else {
		// A new Agent may have been installed after the old node was deleted.
		// Retire only the completed request markers and preserve that new state.
		if data, err := os.ReadFile(agentManagedStatePath + agentUninstallMarkerSuffix); err == nil && strings.TrimSpace(string(data)) == completion.RequestID {
			if err := os.Remove(agentManagedStatePath + agentUninstallMarkerSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if task, err := readAgentUninstallTask(); err == nil && task.NodeGUID == completion.NodeGUID && task.RequestID == completion.RequestID {
			if err := os.Remove(agentManagedStatePath + agentUninstallTaskSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if removeAgentState {
		_ = os.Remove("/var/log/meridian-agent.log")
	}
	paths, err := agentUninstallRecoveryPathsFor(completion.RequestID)
	if err != nil {
		return err
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		if _, statErr := os.Stat(paths.Unit); statErr == nil {
			if err := runAgentUninstallManagerCommand("systemctl", "disable", paths.ServiceName+".service"); err != nil {
				return err
			}
		}
		if err := os.Remove(paths.Unit); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := runAgentUninstallManagerCommand("systemctl", "daemon-reload"); err != nil {
			return err
		}
	} else if _, err := os.Stat("/sbin/openrc-run"); err == nil {
		if output, err := exec.Command("rc-update", "del", paths.ServiceName, "default").CombinedOutput(); err != nil { // #nosec G204 -- ServiceName contains only the fixed prefix and validated lowercase hex request ID.
			listing, listingErr := exec.Command("rc-update", "show", "default").CombinedOutput()
			if listingErr != nil || containsOpenRCService(string(listing), paths.ServiceName) {
				return fmt.Errorf("could not disable the OpenRC uninstall recovery service: %w: %s", err, strings.TrimSpace(string(output)))
			}
		}
		if err := os.Remove(paths.Init); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_ = os.Remove("/var/log/" + paths.ServiceName + ".log")
	}
	if err := os.Remove(paths.Binary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(paths.Done); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func sleepAgentUninstallRetry(attempt int) {
	delay := time.Duration(attempt%6+1) * 5 * time.Second
	time.Sleep(delay)
}

func runAgentUninstallManagerCommand(name string, args ...string) error {
	if name != "systemctl" && name != "rc-service" && name != "rc-update" {
		return errors.New("unsupported Agent uninstall service manager command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is allowlisted above, callers use fixed manager commands and request-scoped validated service names; no shell is involved.
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
}
