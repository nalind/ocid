package server

import (
	"fmt"
	"os"

	"github.com/containers/storage/storage"
	"github.com/mrunalp/ocid/oci"
	"github.com/mrunalp/ocid/utils"
	imagestorage "github.com/nalind/image/storage"
	imagetypes "github.com/nalind/image/types"
	"github.com/rajatchopra/ocicni"
)

const (
	runtimeAPIVersion = "v1alpha1"
	imageStore        = "/var/lib/ocid/images"
	storageRun        = "/var/run/ocid/storage"
	storageGraph      = "/var/lib/ocid/storage"
)

// Server implements the RuntimeService and ImageService
type Server struct {
	runtime      *oci.Runtime
	sandboxDir   string
	state        *serverState
	netPlugin    ocicni.CNIPlugin
	imageContext *imagetypes.SystemContext
	storage      storage.Store
}

// New creates a new Server with options provided
func New(runtimePath, sandboxDir, containerDir string) (*Server, error) {
	// TODO: This will go away later when we have wrapper process or systemd acting as
	// subreaper.
	if err := utils.SetSubreaper(1); err != nil {
		return nil, fmt.Errorf("failed to set server as subreaper: %v", err)
	}

	utils.StartReaper()

	if err := os.MkdirAll(imageStore, 0755); err != nil {
		return nil, err
	}

	r, err := oci.New(runtimePath, containerDir)
	if err != nil {
		return nil, err
	}
	sandboxes := make(map[string]*sandbox)
	containers := make(map[string]*oci.Container)
	store, err := storage.MakeStore(storageRun, storageGraph, "", []string{}, nil, nil)
	if err != nil {
		return nil, err
	}
	imagestorage.Transport.SetStore(store)
	netPlugin, err := ocicni.InitCNI("")
	if err != nil {
		return nil, err
	}
	return &Server{
		runtime:    r,
		netPlugin:  netPlugin,
		sandboxDir: sandboxDir,
		storage:    store,
		state: &serverState{
			sandboxes:  sandboxes,
			containers: containers,
		},
		imageContext: &imagetypes.SystemContext{
			RootForImplicitAbsolutePaths: "",
			SignaturePolicyPath:          "",
			DockerCertPath:               "",
			DockerInsecureSkipTLSVerify:  false,
		},
	}, nil
}

type serverState struct {
	sandboxes  map[string]*sandbox
	containers map[string]*oci.Container
}

type sandbox struct {
	name       string
	logDir     string
	labels     map[string]string
	containers map[string]*oci.Container
}

func (s *Server) addSandbox(sb *sandbox) {
	s.state.sandboxes[sb.name] = sb
}

func (s *Server) hasSandbox(name string) bool {
	_, ok := s.state.sandboxes[name]
	return ok
}

func (s *sandbox) addContainer(c *oci.Container) {
	s.containers[c.Name()] = c
}

func (s *sandbox) removeContainer(c *oci.Container) {
	delete(s.containers, c.Name())
}

func (s *Server) addContainer(c *oci.Container) {
	sandbox := s.state.sandboxes[c.Sandbox()]
	sandbox.addContainer(c)
	s.state.containers[c.Name()] = c
}

func (s *Server) removeContainer(c *oci.Container) {
	sandbox := s.state.sandboxes[c.Sandbox()]
	sandbox.removeContainer(c)
	delete(s.state.containers, c.Name())
}
