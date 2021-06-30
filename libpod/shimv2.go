// +build linux

package libpod

import (
	"context"
	"fmt"
	"io"
	ioutil "io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	cio "github.com/containerd/containerd/pkg/cri/io"
	cioutil "github.com/containerd/containerd/pkg/ioutil"
	client "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/fifo"
	"github.com/containerd/ttrpc"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v3/libpod/define"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	// shimPIDFileName holds the filename
	// that contains the shimv2 daemon PID
	shimPIDFileName = "shim.pid"
	// shimAddressFileName holds the filename
	// that contains the shimv2 socket address
	shimAddressFileName = "address"
)

// shimv2 holds information about the runtime
type shimv2 struct {
	name              string
	path              string
	tmpDir            string
	exitsDir          string
	supportsKVM       bool
	supportsJSON      bool
	supportsNoCgroups bool
	client            *ttrpc.Client
	task              task.TaskService
	ctx               context.Context
	sync.Mutex
	ctrs map[string]containerInfo
}

type containerInfo struct {
	cio *cio.ContainerIO
}

// newShimv2 returns a new instance of shimv2
func newShimv2(name string, paths []string, runtimeFlags []string, runtimeConfig *config.Config) (OCIRuntime, error) {
	logrus.Debug("newShimv2 start")

	r := new(shimv2)

	// set runtime path
	p, err := getRuntimePath(name, paths)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot find a valid OCI runtime path for %q", name)
	}
	r.path = p

	// set runtime name
	r.name = getSupportedBinaryName(r.path)

	// init container info
	r.ctrs = make(map[string]containerInfo)

	// set runtime support
	r.supportsKVM = supportEnabled(r.name, runtimeConfig.Engine.RuntimeSupportsKVM)
	r.supportsJSON = supportEnabled(r.name, runtimeConfig.Engine.RuntimeSupportsJSON)
	r.supportsNoCgroups = supportEnabled(r.name, runtimeConfig.Engine.RuntimeSupportsNoCgroups)

	// set namespace
	r.ctx = namespaces.WithNamespace(context.Background(), namespaces.Default)
	if len(runtimeConfig.Engine.Namespace) > 0 {
		r.ctx = namespaces.WithNamespace(context.Background(), runtimeConfig.Engine.Namespace)
	}

	// create the exits directory to store the exit code
	// returned by stop container operations
	r.tmpDir = runtimeConfig.Engine.TmpDir
	r.exitsDir = filepath.Join(r.tmpDir, "exits")
	if err := os.MkdirAll(r.exitsDir, 0750); err != nil {
		if !os.IsExist(err) {
			return nil, errors.Wrapf(err, "error creating OCI runtime exit directory")
		}
	}

	return r, nil
}

func (r *shimv2) Name() string {
	return r.name
}

func (r *shimv2) Path() string {
	return r.path
}

// connectToSocket creates a shimv2 client with a ready connection to make calls to
// the shimv2 daemon
func (r *shimv2) connectToSocket(ctr *Container, address string) error {
	// get socket address
	if len(address) == 0 {
		// if address is empty, lets read it from the address file
		addressFile := filepath.Join(ctr.bundlePath(), shimAddressFileName)
		b, err := ioutil.ReadFile(addressFile)
		if err != nil {
			return err
		}

		address = string(b)
	}

	// connect to shimv2 socket
	logrus.Debugf("socket address %q", address)
	conn, err := client.Connect(address, client.AnonDialer)
	if err != nil {
		return err
	}

	options := ttrpc.WithOnClose(func() { conn.Close() })
	client := ttrpc.NewClient(conn, options)

	r.client = client
	r.task = task.NewTaskClient(client)

	return nil
}

// AttachSocketPath creates a socket for the container.
// TODO: this is not needed by shimv2 but the func is being called as part of the container init workflow
// so we need to implement it
func (r *shimv2) AttachSocketPath(ctr *Container) (string, error) {
	if ctr == nil {
		return "", errors.Wrapf(define.ErrInvalidArg, "must provide a valid container to get attach socket path")
	}

	return filepath.Join(ctr.bundlePath(), "attach"), nil
}

func (r *shimv2) CreateContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (retErr error) {
	logrus.Debug("CreateContainer start")
	defer logrus.Debug("CreateContainer end")

	ctrID := shortContainerName(ctr)

	// make run dir accessible to the current user so that the PID files can be read without
	// being in the rootless user namespace
	if err := makeAccessible(ctr.state.RunDir, 0, 0); err != nil {
		return err
	}

	// arguments for shimv2 command
	args := []string{"-id", ctrID, "start"}

	// get supported binary name for shimv2 client
	binaryName := getSupportedBinaryName(r.path)

	// create command to run shim v2
	cmd, err := client.Command(r.ctx, binaryName, "", "", ctr.bundlePath(), nil, args...)
	if err != nil {
		return err
	}

	// create the log file expected by shimv2
	f, err := fifo.OpenFifo(r.ctx, filepath.Join(ctr.bundlePath(), "log"),
		unix.O_RDONLY|unix.O_CREAT|unix.O_NONBLOCK, 0o700)
	if err != nil {
		return err
	}

	// copy shimv2 logs to stderr
	go func() {
		defer f.Close()
		if _, err := io.Copy(os.Stderr, f); err != nil {
			logrus.WithError(err).Error("copy shimv2 log")
		}
	}()

	// run command to create shimv2 daemon
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrap(err, string(out))
	}

	// set container monitor pid
	b, err := ioutil.ReadFile(filepath.Join(ctr.bundlePath(), shimPIDFileName))
	if err != nil {
		return errors.Wrapf(err, "cannot read shimv2 PID")
	}

	shimv2PID, err := strconv.Atoi(string(b))
	if err != nil {
		return errors.Wrapf(err, "cannot convert shimv2 PID to int")
	}
	ctr.state.ConmonPID = shimv2PID

	// get shimv2 socket address
	address := strings.TrimSpace(string(out))

	// Save address
	// a := []byte(address)
	// addressFile := filepath.Join(ctr.bundlePath(), "address")
	// err = ioutil.WriteFile(addressFile, a, 0644)
	// if err != nil {
	// 	return errors.Wrapf(err, "socket address file could not be created")
	// }

	// connect to shimv2 socket address
	if err := r.connectToSocket(ctr, address); err != nil {
		return errors.Wrap(err, "cannot connect to shimv2 socket")
	}
	// conn, err := client.Connect(address, client.AnonDialer)
	// if err != nil {
	// 	return err
	// }

	// options := ttrpc.WithOnClose(func() { conn.Close() })
	// tc := ttrpc.NewClient(conn, options)

	// r.client = tc
	// r.task = task.NewTaskClient(tc)

	// Create container IO
	// TODO: this might be the wrong root path for the IO
	containerIO, err := cio.NewContainerIO(ctrID,
		cio.WithNewFIFOs(ctr.config.StaticDir, ctr.config.Spec.Process.Terminal, ctr.config.Stdin))
	if err != nil {
		return err
	}

	defer func() {
		if retErr != nil {
			containerIO.Close()
		}
	}()

	// redirect ctr stdio to ctr log
	// TODO: this might be the wrong root path for the ctr log
	if len(ctr.config.LogPath) == 0 {
		ctr.config.LogPath = filepath.Join(ctr.config.StaticDir, "ctr.log")
	}
	f, err = os.OpenFile(ctr.LogPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}

	var stdoutCh, stderrCh <-chan struct{}
	wc := cioutil.NewSerialWriteCloser(f)
	stdout, stdoutCh := cio.NewCRILogger(ctr.LogPath(), wc, cio.Stdout, -1)
	stderr, stderrCh := cio.NewCRILogger(ctr.LogPath(), wc, cio.Stderr, -1)

	go func() {
		if stdoutCh != nil {
			<-stdoutCh
		}
		if stderrCh != nil {
			<-stderrCh
		}
		logrus.Debugf("finish redirecting log file %s, closing it", ctr.LogPath())
		f.Close()
	}()

	containerIO.AddOutput(ctr.LogPath(), stdout, stderr)
	containerIO.Pipe()

	r.Lock()
	r.ctrs[ctrID] = containerInfo{
		cio: containerIO,
	}
	r.Unlock()

	defer func() {
		if retErr != nil {
			logrus.Debugf("Cleaning up container %s: %v", ctrID, err)
			if cleanupErr := r.DeleteContainer(ctr); cleanupErr != nil {
				logrus.Debugf("DeleteContainer failed for container %s: %v", ctrID, cleanupErr)
			}
		}
	}()

	// create request for container creation
	request := &task.CreateTaskRequest{
		ID:       ctrID,
		Bundle:   ctr.bundlePath(),
		Stdin:    containerIO.Config().Stdin,
		Stdout:   containerIO.Config().Stdout,
		Stderr:   containerIO.Config().Stderr,
		Terminal: containerIO.Config().Terminal,
	}

	// create container
	createdCh := make(chan error)
	go func() {
		if resp, err := r.task.Create(r.ctx, request); err != nil {
			createdCh <- errdefs.FromGRPC(err)
		} else {
			ctr.state.PID = int(resp.Pid)
		}
		close(createdCh)
	}()

	// check container creation error
	select {
	case err = <-createdCh:
		if err != nil {
			return errors.Errorf("container creation failed: %v", err)
		}
	case <-time.After(define.ContainerCreateTimeout):
		if err := r.DeleteContainer(ctr); err != nil {
			return err
		}
		<-createdCh
		return errors.Errorf("container creation timeout (%v)", define.ContainerCreateTimeout)
	}

	return nil
}

func (r *shimv2) closeIO(ctrID, execID string) error {
	_, err := r.task.CloseIO(r.ctx, &task.CloseIORequest{
		ID:     ctrID,
		ExecID: execID,
		Stdin:  true,
	})
	if err != nil {
		return errdefs.FromGRPC(err)
	}

	return nil
}

// AttachContainer attaches stdio to container stdio
func (r *shimv2) AttachContainer(ctr *Container, inputStream io.Reader, outputStream, errorStream io.WriteCloser, tty bool) error {
	logrus.Debug("AttachContainer start")

	// Initialize terminal resizing
	// kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
	// 	log.Debugf(ctx, "Got a resize event: %+v", size)

	// 	if err := r.resizePty(c.ID(), "", size); err != nil {
	// 		log.Warnf(ctx, "Failed to resize terminal: %v", err)
	// 	}
	// })

	ctrID := shortContainerName(ctr)

	// connect to shimv2 socket
	if r.task == nil {
		if err := r.connectToSocket(ctr, ""); err != nil {
			return errors.Wrapf(err, "cannot connect to shimv2 socket")
		}
	}

	r.Lock()
	cInfo, ok := r.ctrs[ctrID]
	r.Unlock()
	if !ok {
		return errors.New("Could not retrieve container information")
	}

	opts := cio.AttachOptions{
		Stdin:     inputStream,
		Stdout:    outputStream,
		Stderr:    errorStream,
		Tty:       tty,
		StdinOnce: ctr.Stdin(),
		CloseStdin: func() error {
			return r.closeIO(ctrID, "")
		},
	}

	cInfo.cio.Attach(opts)

	return nil
}

func (r *shimv2) StartContainer(ctr *Container) error {
	logrus.Debug("StartContainer start")
	defer logrus.Debug("StartContainer end")

	ctrID := shortContainerName(ctr)

	// check if connection exists
	if r.task == nil {
		if err := r.connectToSocket(ctr, ""); err != nil {
			return errors.Wrapf(err, "cannot connect to shimv2 socket")
		}
	}

	response, err := r.task.Start(r.ctx, &task.StartRequest{
		ID:     ctrID,
		ExecID: "",
	})

	if err != nil {
		return errdefs.FromGRPC(err)
	}

	logrus.Debugf("response PID: %d", response.Pid)
	ctr.state.StartedTime = time.Now()

	// check if container was created and update its state
	go func() {
		err := r.wait(ctrID, "")
		if err == nil {
			if err1 := r.UpdateContainerStatus(ctr); err1 != nil {
				logrus.Debugf("error updating container state: %v", err1)
			}
		} else {
			logrus.Debugf("error waiting for container %s: %v", ctr.ID(), err)
		}
	}()

	return nil
}

func (r *shimv2) KillContainer(ctr *Container, signal uint, all bool) error {
	logrus.Debug("KillContainer start")
	defer logrus.Debug("KillContainer end")

	// connect to shimv2 socket
	if r.task == nil {
		if err := r.connectToSocket(ctr, ""); err != nil {
			return errors.Wrapf(err, "cannot connect to shimv2 socket")
		}
	}

	if _, err := r.task.Kill(r.ctx, &task.KillRequest{
		ID:     shortContainerName(ctr),
		ExecID: "",
		Signal: uint32(signal),
		All:    all,
	}); err != nil {
		logrus.Error(err)
		return errdefs.FromGRPC(err)
	}

	return nil
}

func (r *shimv2) StopContainer(ctr *Container, timeout uint, all bool) error {
	logrus.Debug("StopContainer start")
	defer logrus.Debug("StopContainer end")

	ctrID := shortContainerName(ctr)

	// update container state once it has been stopped
	defer func() {
		exitCode := -1
		if err := r.UpdateContainerStatus(ctr); err != nil {
			logrus.Errorf("error updating container state: %v", err)
		} else {
			exitCode = int(ctr.state.ExitCode)
		}

		// podman expects a /run/libpod/exits/ctrID file with an exit code
		if err := createExitsFile(ctr, exitCode); err != nil {
			logrus.Errorf("error creating exits file: %v", err)
		}
	}()

	// connect to shimv2 socket
	if r.task == nil {
		if err := r.connectToSocket(ctr, ""); err != nil {
			return errors.Wrapf(err, "cannot connect to shimv2 socket")
		}
	}

	// Ping the container to see if it's alive, otherwise it's already stopped
	// fmt.Println(ctr.state.PID)
	// err := unix.Kill(ctr.state.PID, 0)
	// if err == unix.ESRCH {
	// 	fmt.Println("PING ERROR")
	// 	//return nil
	// }

	// // Cancel the context before returning to ensure goroutines are stopped.
	// ctx, cancel := context.WithCancel(r.ctx)
	// defer cancel()

	// wait for container to be stopped
	errCh := make(chan error)
	stopCh := make(chan struct{})
	go func() {
		select {
		case <-stopCh:
			return
		default:
			// errdefs.ErrNotFound actually comes from a closed connection, which is expected
			// when stoping the container, with the agent and the VM going off. In such case
			// let's just ignore the error
			err := r.wait(ctrID, "")
			if err != nil && !errors.Is(err, errdefs.ErrNotFound) {
				errCh <- errdefs.FromGRPC(err)
			}

			close(errCh)
		}
	}()

	// get stop signal from config
	stopSignal := ctr.config.StopSignal
	if stopSignal == 0 {
		stopSignal = uint(syscall.SIGTERM)
	}

	if timeout > 0 {
		logrus.Debugf("kill container with signal %d and timeout %v", stopSignal, timeout)
		if err := r.KillContainer(ctr, stopSignal, all); err != nil {
			// Let's check if the container is stopped
			// err := unix.Kill(ctr.state.PID, 0)
			// if err == unix.ESRCH {
			// 	fmt.Println("PING 2")
			// 	//return nil
			// }

			return errors.Wrapf(err, "error sending signal %d to container %s", stopSignal, ctr.ID())
		}

		// give runtime a few seconds to do the friendly termination
		if err := waitCtrTerminate(stopSignal, stopCh, errCh, time.Duration(timeout)*time.Second, false); err != nil {
			logrus.Debugf("timed out stopping container %s: %v", ctr.ID(), err)
		} else {
			return nil
		}
	}

	stopSignal = 9
	logrus.Debugf("kill container with signal %d and default timeout %v", stopSignal, killContainerTimeout)
	if err := r.KillContainer(ctr, stopSignal, all); err != nil {
		// Again, check if the container is gone. If it is, exit cleanly.
		// err := unix.Kill(ctr.state.PID, 0)
		// if err == unix.ESRCH {
		// 	fmt.Println("PING 3")
		// 	//return nil
		// }

		return errors.Wrapf(err, "error sending signal %d to container %s", stopSignal, ctr.ID())
	}

	// give runtime a few seconds to do the hard termination
	if err := waitCtrTerminate(stopSignal, stopCh, errCh, killContainerTimeout, true); err != nil {
		return errors.Wrapf(err, "error waiting for container termination")
	}

	return nil
}

func (r *shimv2) DeleteContainer(ctr *Container) error {
	logrus.Debug("DeleteContainer start")
	defer logrus.Debug("DeleteContainer end")

	ctrID := shortContainerName(ctr)

	// connect to shimv2 socket
	if r.task == nil {
		if err := r.connectToSocket(ctr, ""); err != nil {
			return errors.Wrapf(err, "cannot connect to shimv2 socket")
		}
	}

	// TODO: close container IO

	// run delete task
	if _, err := r.task.Delete(r.ctx, &task.DeleteRequest{
		ID:     ctrID,
		ExecID: "",
	}); err != nil && !errors.Is(err, ttrpc.ErrClosed) {
		return errdefs.FromGRPC(err)
	}

	// run shutdown task
	_, err := r.task.Shutdown(r.ctx, &task.ShutdownRequest{
		ID: ctrID,
	})
	if err != nil && !errors.Is(err, ttrpc.ErrClosed) {
		return err
	}

	return nil
}

func (r *shimv2) PauseContainer(ctr *Container) error {
	return fmt.Errorf("not supported")
}

func (r *shimv2) UnpauseContainer(ctr *Container) error {
	return fmt.Errorf("not supported")
}

func (r *shimv2) HTTPAttach(ctr *Container, req *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, detachKeys *string, cancel <-chan bool, hijackDone chan<- bool, streamAttach, streamLogs bool) (deferredErr error) {
	return fmt.Errorf("not supported")
}

func (r *shimv2) AttachResize(ctr *Container, newSize define.TerminalSize) error {
	return fmt.Errorf("not supported")
}

func (r *shimv2) CheckpointContainer(ctr *Container, options ContainerCheckpointOptions) error {
	return fmt.Errorf("not supported")
}

// since conmon is not needed here, let's mock the response as true.
// TODO: the shimv2 PID could be checked to get its state
func (r *shimv2) CheckConmonRunning(ctr *Container) (bool, error) {
	return true, nil
}

func (r *shimv2) SupportsCheckpoint() bool {
	return false
}

func (r *shimv2) SupportsJSONErrors() bool {
	return false
}

func (r *shimv2) SupportsNoCgroups() bool {
	return false
}

func (r *shimv2) SupportsKVM() bool {
	return r.supportsKVM
}

func (r *shimv2) ExitFilePath(ctr *Container) (string, error) {
	if ctr == nil {
		return "", errors.Wrapf(define.ErrInvalidArg, "must provide a valid container to get exit file path")
	}
	return filepath.Join(r.exitsDir, ctr.ID()), nil
}

func (r *shimv2) RuntimeInfo() (*define.ConmonInfo, *define.OCIRuntimeInfo, error) {
	return nil, nil, fmt.Errorf("not supported")
}

func (r *shimv2) ExecContainerHTTP(ctr *Container, sessionID string, options *ExecOptions, req *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool) (int, chan error, error) {
	return 0, nil, fmt.Errorf("not supported")
}

func (r *shimv2) ExecContainer(c *Container, sessionID string, options *ExecOptions, streams *define.AttachStreams) (int, chan error, error) {
	return 0, nil, fmt.Errorf("not supported")
}

func (r *shimv2) ExecContainerDetached(ctr *Container, sessionID string, options *ExecOptions, stdin bool) (int, error) {
	return 0, fmt.Errorf("not supported")
}

func (r *shimv2) ExecAttachResize(ctr *Container, sessionID string, newSize define.TerminalSize) error {
	return fmt.Errorf("not supported")
}

func (r *shimv2) ExecStopContainer(ctr *Container, sessionID string, timeout uint) error {
	return fmt.Errorf("not supported")
}

func (r *shimv2) ExecUpdateStatus(ctr *Container, sessionID string) (bool, error) {
	return false, fmt.Errorf("not supported")
}

func (r *shimv2) ExecAttachSocketPath(ctr *Container, sessionID string) (string, error) {
	return "", fmt.Errorf("not supported")
}

func (r *shimv2) UpdateContainerStatus(ctr *Container) error {
	logrus.Debug("updateContainerStatus start")
	defer logrus.Debug("updateContainerStatus end")

	if r.task == nil {
		return errors.New("runtime not correctly setup")
	}

	ctrID := shortContainerName(ctr)

	response, err := r.task.State(r.ctx, &task.StateRequest{
		ID: ctrID,
	})
	if err != nil {
		if !errors.Is(err, ttrpc.ErrClosed) {
			return errdefs.FromGRPC(err)
		}
		return errdefs.ErrNotFound
	}

	switch response.Status {
	case tasktypes.StatusCreated:
		ctr.state.State = define.ContainerStateCreated
	case tasktypes.StatusRunning:
		ctr.state.State = define.ContainerStateRunning
	case tasktypes.StatusStopped:
		ctr.state.State = define.ContainerStateStopped
	case tasktypes.StatusPaused:
		ctr.state.State = define.ContainerStatePaused
	default:
		return errors.Wrapf(define.ErrInternal, "unrecognized status returned for container %s: %s",
			ctr.ID(), response.Status)
	}

	ctr.state.FinishedTime = response.ExitedAt
	ctr.state.PID = int(response.Pid)
	exitCode := int32(response.ExitStatus)
	ctr.state.ExitCode = exitCode

	if exitCode != 0 {
		oomFilePath := filepath.Join(ctr.bundlePath(), "oom")
		if _, err = os.Stat(oomFilePath); err == nil {
			ctr.state.OOMKilled = true
		}
	}
	return nil
}

func (r *shimv2) wait(ctrID, execID string) error {
	logrus.Debug("wait start")
	defer logrus.Debug("wait end")

	_, err := r.task.Wait(r.ctx, &task.WaitRequest{
		ID:     ctrID,
		ExecID: execID,
	})
	if err != nil {
		logrus.Errorf("error running task wait: %v", err)
		if !errors.Is(err, ttrpc.ErrClosed) {
			return errdefs.FromGRPC(err)
		}
		return errdefs.ErrNotFound
	}

	return nil
}

func waitCtrTerminate(sig uint, stopCh chan struct{}, errCh chan error, timeout time.Duration, cancelWait bool) error {
	logrus.Debug("waitCtrTerminate start")
	defer logrus.Debug("waitCtrTerminate end")

	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		if cancelWait {
			close(stopCh)
		}
		return errors.Errorf("stop container with signal %d timed out after %v", sig, timeout)
	}
}

// createExitsFile creates the exits file expected by podman to get the exit code
// returned by the container stop operation
func createExitsFile(ctr *Container, exitCode int) error {
	logrus.Debug("createExitsFile start")
	defer logrus.Debug("createExitsFile end")

	exitFile, err := ctr.exitFilePath()
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(exitFile, []byte(strconv.Itoa(exitCode)), 0777); err != nil {
		return err
	}

	return nil
}

// shortContainerName returns the first 12 characteres of the containers'name.
// This is required due to the error: unix socket path too long shim v2, socket path must be < 106 characters.
// TODO: better logic to handle this since namespace could variate
func shortContainerName(ctr *Container) string {
	return ctr.ID()[:12]
}

// supportEnabled validates if the runtime is part of the runtime group that
// supports an specific capability
func supportEnabled(runtimeName string, supportedRuntimes []string) bool {
	s := make(map[string]bool, len(supportedRuntimes))

	for _, r := range supportedRuntimes {
		s[r] = true
	}

	return s[runtimeName]
}

// getRuntimePath returns the first valid path from the list of paths
// where binary is located
func getRuntimePath(name string, paths []string) (string, error) {
	for _, path := range paths {
		stat, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if !stat.Mode().IsRegular() {
			continue
		}
		return path, nil
	}

	// Search the $PATH as last fallback
	if foundRuntime, err := exec.LookPath(name); err == nil {
		return foundRuntime, nil
	}

	return "", errors.Wrapf(define.ErrInvalidArg, "no valid executable found for OCI runtime %s", name)
}

// getSupportedBinaryName transforms the binary so that it has the
// correct name expected by the shimv2 client
func getSupportedBinaryName(path string) string {
	runtime := filepath.Base(path)
	return strings.Replace(runtime, "-", ".", -1)
}
