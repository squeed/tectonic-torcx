package internal

import (
	"io/ioutil"
	"strings"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"github.com/coreos/container-linux-update-operator/pkg/updateengine"
)

// Avoid a race condition where this chan gets closed / gc'd while another
// G still has an outstanding message by declaring this global
var statusCh chan updateengine.Status

// OsReleaseFile contains the default path to the os-release file
const OsReleaseFile = "/usr/lib/os-release"

// GetCurrentOSVersion adds the current OS version to the OSVersions list
func (a *App) GetCurrentOSVersion() error {
	logrus.Debug("reading current OS version from " + OsReleaseFile)
	osr, err := ioutil.ReadFile(OsReleaseFile)
	if err != nil {
		return errors.Wrap(err, "could not read os-release file")
	}

	version := parseOSVersion(string(osr))
	if version == "" {
		return errors.New("invalid os-release file")
	}

	a.OSVersions = append(a.OSVersions, version)

	return nil
}

func parseOSVersion(releaseInfo string) string {
	lines := strings.Split(releaseInfo, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "VERSION=") {
			return strings.SplitN(line, "=", 2)[1]
		}
	}
	return ""
}

// NextOSVersion gets the coming OS version from update_engine
// without changing anything.
func (a *App) NextOSVersion() error {
	logrus.Debug("Requesting next OS version")
	ue, err := updateengine.New()
	if err != nil {
		return errors.Wrapf(err, "failed to connect to update-engine")
	}
	defer ue.Close()

	status, err := ue.GetStatus()
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve update-engine status")
	}

	if status.CurrentOperation == updateengine.UpdateStatusUpdatedNeedReboot {
		logrus.Infof("Next OS version is %s", status.NewVersion)
		a.OSVersions = append(a.OSVersions, status.NewVersion)
	} else {
		logrus.Debugf("update_engine status is %s, cannot determine next version", status.CurrentOperation)
	}
	return nil
}

// OSUpdate triggers the update engine to update and waits
// for it to finish
func (a *App) OSUpdate() error {
	logrus.Infof("Updating node OS")
	var err error

	// Connect to ue dbus api
	ue, err := updateengine.New()
	if err != nil {
		return errors.Wrap(err, "Failed to connect to update-engine")
	}
	defer ue.Close()

	logrus.Info("Triggering OS update")
	// Trigger check for update. This is non-blocking
	if err := ue.AttemptUpdate(); err != nil {
		return errors.Wrap(err, "failed to trigger update")
	}

	logrus.Debug("Waiting for update to finish")
	if err := a.waitForUpdate(ue); err != nil {
		return errors.Wrap(err, "failed to wait for update to complete")
	}

	return nil
}

// waitForUpdate watches the status channel and waits until
// it seems complete.
func (a *App) waitForUpdate(ue *updateengine.Client) error {
	statusCh = make(chan updateengine.Status, 10)
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	go func() {
		// use a waitgroup to fix a fun race condition where both
		// stopch and the client are closed, causing a panic
		wg.Add(1)
		ue.ReceiveStatuses(statusCh, stopCh)
		wg.Done()
	}()

	// Manually send the current status on the channel as a "start" marker
	firstStatus, err := ue.GetStatus()
	if err != nil {
		close(stopCh)
		return errors.Wrap(err, "failed to get status")
	}

	firstStatus.NewSize = -1 // hack
	statusCh <- firstStatus

	flushed := false
loop:
	for status := range statusCh {

		// The updateengine client starts queueing statuses as soon as
		// the connection is opened. Flush the channel until our manual
		// status is received. This is so we ignore any old errors.
		if !flushed && status.NewSize != -1 {
			flushed = true
			continue
		}

		logrus.Debug("current status: ", status.CurrentOperation, " ", status.NewVersion)

		switch status.CurrentOperation {
		case updateengine.UpdateStatusCheckingForUpdate, updateengine.UpdateStatusUpdateAvailable, updateengine.UpdateStatusDownloading, updateengine.UpdateStatusVerifying, updateengine.UpdateStatusFinalizing:
			// pass; update still in progress

		case updateengine.UpdateStatusUpdatedNeedReboot:
			// Update complete, reboot time
			logrus.Info("Update successful! Next version is ", status.NewVersion)
			a.OSVersions = append(a.OSVersions, status.NewVersion)
			a.NeedReboot = true
			break loop

		case updateengine.UpdateStatusIdle:
			// already up to date, no reboot needed
			break loop

		case updateengine.UpdateStatusReportingErrorEvent:
			// TODO: determine if we care about errors
		}
	}

	close(stopCh)
	wg.Wait()
	return nil
}
