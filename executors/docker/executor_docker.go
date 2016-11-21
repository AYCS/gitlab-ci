package docker

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/fsouza/go-dockerclient"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/executors"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
	docker_helpers "gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers/docker"
)

type dockerOptions struct {
	Image    string   `json:"image"`
	Services []string `json:"services"`
}

type executor struct {
	executors.AbstractExecutor
	client      docker_helpers.Client
	failures    []*docker.Container
	builds      []*docker.Container
	services    []*docker.Container
	caches      []*docker.Container
	options     dockerOptions
	info        *docker.Env
	binds       []string
	volumesFrom []string
	devices     []docker.Device
	links       []string
}

func (s *executor) getServiceVariables() []string {
	return s.Build.GetAllVariables().PublicOrInternal().StringList()
}

func (s *executor) getAuthConfig(imageName string) (docker.AuthConfiguration, error) {
	authConfigResolver := docker_helpers.NewAuthConfigResolver()

	err := authConfigResolver.ReadHomeDirectoryAuthConfig(s.Shell().User)
	if err != nil {
		return docker.AuthConfiguration{}, err
	}

	if s.Build != nil {
		err = authConfigResolver.ReadStringAuthConfig(s.Build.GetDockerAuthConfigs())
		if err != nil {
			return docker.AuthConfiguration{}, err
		}

		authConfigResolver.AddConfiguration(s.Build.RegistryURL, "gitlab-ci-token", s.Build.Token)
	}

	authConfig, indexName := authConfigResolver.ResolveAuthConfig(imageName)
	if authConfig != nil {
		s.Debugln("Using", authConfig.Username, "to connect to", authConfig.ServerAddress, "in order to resolve", imageName, "...")
		return *authConfig, nil
	}

	return docker.AuthConfiguration{}, fmt.Errorf("No credentials found for %v", indexName)
}

func (s *executor) pullDockerImage(imageName string) (*docker.Image, error) {
	s.Println("Pulling docker image", imageName, "...")
	authConfig, err := s.getAuthConfig(imageName)
	if err != nil {
		s.Debugln(err)
	}

	pullImageOptions := docker.PullImageOptions{
		Repository: imageName,
	}

	// Add :latest to limit the download results
	if !strings.ContainsAny(pullImageOptions.Repository, ":@") {
		pullImageOptions.Repository += ":latest"
	}

	err = s.client.PullImage(pullImageOptions, authConfig)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, &common.BuildError{Inner: err}
		}
		return nil, err
	}

	image, err := s.client.InspectImage(imageName)
	return image, err
}

func (s *executor) getDockerImage(imageName string) (*docker.Image, error) {
	pullPolicy, err := s.Config.Docker.PullPolicy.Get()
	if err != nil {
		return nil, err
	}

	s.Debugln("Looking for image", imageName, "...")
	image, err := s.client.InspectImage(imageName)

	// If never is specified then we return what inspect did return
	if pullPolicy == common.PullPolicyNever {
		return image, err
	}

	if err == nil {
		// Don't pull image that is passed by ID
		if image.ID == imageName {
			return image, nil
		}

		// If not-present is specified
		if pullPolicy == common.PullPolicyIfNotPresent {
			return image, err
		}
	}

	newImage, err := s.pullDockerImage(imageName)
	if err != nil {
		if image != nil {
			s.Warningln("Cannot pull the latest version of image", imageName, ":", err)
			s.Warningln("Locally found image will be used instead.")
			return image, nil
		}
		return nil, err
	}
	return newImage, nil
}

func (s *executor) getArchitecture() string {
	architecture := s.info.Get("Architecture")
	switch architecture {
	case "armv7l", "aarch64":
		architecture = "arm"
	case "amd64":
		architecture = "x86_64"
	}

	if architecture != "" {
		return architecture
	}

	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

func (s *executor) getPrebuiltImage() (image *docker.Image, err error) {
	architecture := s.getArchitecture()
	if architecture == "" {
		return nil, errors.New("unsupported docker architecture")
	}

	imageName := prebuiltImageName + "-" + architecture + ":" + common.REVISION
	s.Debugln("Looking for prebuilt image", imageName, "...")
	image, err = s.client.InspectImage(imageName)
	if err == nil {
		return
	}

	data, err := Asset("prebuilt-" + architecture + prebuiltImageExtension)
	if err != nil {
		return nil, fmt.Errorf("Unsupported architecture: %s: %q", architecture, err.Error())
	}

	s.Debugln("Loading prebuilt image...")
	err = s.client.ImportImage(docker.ImportImageOptions{
		Repository:  prebuiltImageName + "-" + architecture,
		Tag:         common.REVISION,
		Source:      "-",
		InputStream: bytes.NewBuffer(data),
	})
	if err != nil {
		return
	}

	return s.client.InspectImage(imageName)
}

func (s *executor) getAbsoluteContainerPath(dir string) string {
	if path.IsAbs(dir) {
		return dir
	}
	return path.Join(s.Build.FullProjectDir(), dir)
}

func (s *executor) addHostVolume(hostPath, containerPath string) error {
	containerPath = s.getAbsoluteContainerPath(containerPath)
	s.Debugln("Using host-based", hostPath, "for", containerPath, "...")
	s.binds = append(s.binds, fmt.Sprintf("%v:%v", hostPath, containerPath))
	return nil
}

func (s *executor) getLabels(containerType string, otherLabels ...string) map[string]string {
	labels := make(map[string]string)
	labels[dockerLabelPrefix+".build.id"] = strconv.Itoa(s.Build.ID)
	labels[dockerLabelPrefix+".build.sha"] = s.Build.Sha
	labels[dockerLabelPrefix+".build.before_sha"] = s.Build.BeforeSha
	labels[dockerLabelPrefix+".build.ref_name"] = s.Build.RefName
	labels[dockerLabelPrefix+".project.id"] = strconv.Itoa(s.Build.ProjectID)
	labels[dockerLabelPrefix+".runner.id"] = s.Build.Runner.ShortDescription()
	labels[dockerLabelPrefix+".runner.local_id"] = strconv.Itoa(s.Build.RunnerID)
	labels[dockerLabelPrefix+".type"] = containerType
	for _, label := range otherLabels {
		keyValue := strings.SplitN(label, "=", 2)
		if len(keyValue) == 2 {
			labels[dockerLabelPrefix+"."+keyValue[0]] = keyValue[1]
		}
	}
	return labels
}

func (s *executor) createCacheVolume(containerName, containerPath string) (*docker.Container, error) {
	// get busybox image
	cacheImage, err := s.getPrebuiltImage()
	if err != nil {
		return nil, err
	}

	createContainerOptions := docker.CreateContainerOptions{
		Name: containerName,
		Config: &docker.Config{
			Image: cacheImage.ID,
			Cmd: []string{
				"gitlab-runner-cache", containerPath,
			},
			Volumes: map[string]struct{}{
				containerPath: {},
			},
			Labels: s.getLabels("cache", "cache.dir="+containerPath),
		},
		HostConfig: &docker.HostConfig{
			LogConfig: docker.LogConfig{
				Type: "json-file",
			},
		},
	}

	container, err := s.client.CreateContainer(createContainerOptions)
	if err != nil {
		if container != nil {
			s.failures = append(s.failures, container)
		}
		return nil, err
	}

	s.Debugln("Starting cache container", container.ID, "...")
	err = s.client.StartContainer(container.ID, nil)
	if err != nil {
		s.failures = append(s.failures, container)
		return nil, err
	}

	s.Debugln("Waiting for cache container", container.ID, "...")
	errorCode, err := s.client.WaitContainer(container.ID)
	if err != nil {
		s.failures = append(s.failures, container)
		return nil, err
	}

	if errorCode != 0 {
		s.failures = append(s.failures, container)
		return nil, fmt.Errorf("cache container for %s returned %d", containerPath, errorCode)
	}

	return container, nil
}

func (s *executor) addCacheVolume(containerPath string) error {
	var err error
	containerPath = s.getAbsoluteContainerPath(containerPath)

	// disable cache for automatic container cache, but leave it for host volumes (they are shared on purpose)
	if s.Config.Docker.DisableCache {
		s.Debugln("Container cache for", containerPath, " is disabled.")
		return nil
	}

	hash := md5.Sum([]byte(containerPath))

	// use host-based cache
	if cacheDir := s.Config.Docker.CacheDir; cacheDir != "" {
		hostPath := fmt.Sprintf("%s/%s/%x", cacheDir, s.Build.ProjectUniqueName(), hash)
		hostPath, err := filepath.Abs(hostPath)
		if err != nil {
			return err
		}
		s.Debugln("Using path", hostPath, "as cache for", containerPath, "...")
		s.binds = append(s.binds, fmt.Sprintf("%v:%v", filepath.ToSlash(hostPath), containerPath))
		return nil
	}

	// get existing cache container
	containerName := fmt.Sprintf("%s-cache-%x", s.Build.ProjectUniqueName(), hash)
	container, _ := s.client.InspectContainer(containerName)

	// check if we have valid cache, if not remove the broken container
	if container != nil && container.Volumes[containerPath] == "" {
		s.removeContainer(container.ID)
		container = nil
	}

	// create new cache container for that project
	if container == nil {
		container, err = s.createCacheVolume(containerName, containerPath)
		if err != nil {
			return err
		}
	}

	s.Debugln("Using container", container.ID, "as cache", containerPath, "...")
	s.volumesFrom = append(s.volumesFrom, container.ID)
	return nil
}

func (s *executor) addVolume(volume string) error {
	var err error
	hostVolume := strings.SplitN(volume, ":", 2)
	switch len(hostVolume) {
	case 2:
		err = s.addHostVolume(hostVolume[0], hostVolume[1])

	case 1:
		// disable cache disables
		err = s.addCacheVolume(hostVolume[0])
	}

	if err != nil {
		s.Errorln("Failed to create container volume for", volume, err)
	}
	return err
}

func (s *executor) createBuildVolume() (err error) {
	// Cache Git sources:
	// take path of the projects directory,
	// because we use `rm -rf` which could remove the mounted volume
	parentDir := path.Dir(s.Build.FullProjectDir())

	if !path.IsAbs(parentDir) && parentDir != "/" {
		return errors.New("build directory needs to be absolute and non-root path")
	}

	if s.isHostMountedVolume(s.Build.RootDir, s.Config.Docker.Volumes...) {
		return nil
	}

	if s.Build.GetGitStrategy() == common.GitFetch && !s.Config.Docker.DisableCache {
		// create persistent cache container
		err = s.addVolume(parentDir)
	} else {
		var container *docker.Container

		// create temporary cache container
		container, err = s.createCacheVolume("", parentDir)
		if container != nil {
			s.caches = append(s.caches, container)
			s.volumesFrom = append(s.volumesFrom, container.ID)
		}
	}

	return
}

func (s *executor) createUserVolumes() (err error) {
	for _, volume := range s.Config.Docker.Volumes {
		err = s.addVolume(volume)
		if err != nil {
			return
		}
	}
	return nil
}

func (s *executor) isHostMountedVolume(dir string, volumes ...string) bool {
	isParentOf := func(parent string, dir string) bool {
		for dir != "/" && dir != "." {
			if dir == parent {
				return true
			}
			dir = path.Dir(dir)
		}
		return false
	}

	for _, volume := range volumes {
		hostVolume := strings.Split(volume, ":")
		if len(hostVolume) < 2 {
			continue
		}

		if isParentOf(path.Clean(hostVolume[1]), path.Clean(dir)) {
			return true
		}
	}
	return false
}

func (s *executor) parseDeviceString(deviceString string) (device docker.Device, err error) {
	// Split the device string PathOnHost[:PathInContainer[:CgroupPermissions]]
	parts := strings.Split(deviceString, ":")

	if len(parts) > 3 {
		err = fmt.Errorf("Too many colons")
		return
	}

	device.PathOnHost = parts[0]

	// Optional container path
	if len(parts) >= 2 {
		device.PathInContainer = parts[1]
	} else {
		// default: device at same path in container
		device.PathInContainer = device.PathOnHost
	}

	// Optional permissions
	if len(parts) >= 3 {
		device.CgroupPermissions = parts[2]
	} else {
		// default: rwm, just like 'docker run'
		device.CgroupPermissions = "rwm"
	}

	return
}

func (s *executor) bindDevices() (err error) {
	for _, deviceString := range s.Config.Docker.Devices {
		device, err := s.parseDeviceString(deviceString)
		if err != nil {
			err = fmt.Errorf("Failed to parse device string %q: %s", deviceString, err)
			return err
		}

		s.devices = append(s.devices, device)
	}
	return nil
}

func (s *executor) splitServiceAndVersion(serviceDescription string) (service, version, imageName string, linkNames []string) {
	ReferenceRegexpNoPort := regexp.MustCompile(`^(.*?)(|:[0-9]+)(|/.*)$`)
	imageName = serviceDescription
	version = "latest"

	if match := reference.ReferenceRegexp.FindStringSubmatch(serviceDescription); match != nil {
		matchService := ReferenceRegexpNoPort.FindStringSubmatch(match[1])
		service = matchService[1] + matchService[3]

		if len(match[2]) > 0 {
			version = match[2]
		} else {
			imageName = match[1] + ":" + version
		}
	} else {
		return
	}

	linkName := strings.Replace(service, "/", "__", -1)
	linkNames = append(linkNames, linkName)

	// Create alternative link name according to RFC 1123
	// Where you can use only `a-zA-Z0-9-`
	if alternativeName := strings.Replace(service, "/", "-", -1); linkName != alternativeName {
		linkNames = append(linkNames, alternativeName)
	}
	return
}

func (s *executor) createService(service, version, image string) (*docker.Container, error) {
	if len(service) == 0 {
		return nil, errors.New("invalid service name")
	}

	serviceImage, err := s.getDockerImage(image)
	if err != nil {
		return nil, err
	}

	containerName := s.Build.ProjectUniqueName() + "-" + strings.Replace(service, "/", "__", -1)

	// this will fail potentially some builds if there's name collision
	s.removeContainer(containerName)

	s.Println("Starting service", service+":"+version, "...")
	createContainerOpts := docker.CreateContainerOptions{
		Name: containerName,
		Config: &docker.Config{
			Image:  serviceImage.ID,
			Labels: s.getLabels("service", "service="+service, "service.version="+version),
			Env:    s.getServiceVariables(),
		},
		HostConfig: &docker.HostConfig{
			RestartPolicy: docker.NeverRestart(),
			Privileged:    s.Config.Docker.Privileged,
			NetworkMode:   s.Config.Docker.NetworkMode,
			Binds:         s.binds,
			VolumesFrom:   s.volumesFrom,
			LogConfig: docker.LogConfig{
				Type: "json-file",
			},
		},
	}

	s.Debugln("Creating service container", createContainerOpts.Name, "...")
	container, err := s.client.CreateContainer(createContainerOpts)
	if err != nil {
		return nil, err
	}

	s.Debugln("Starting service container", container.ID, "...")
	err = s.client.StartContainer(container.ID, nil)
	if err != nil {
		s.failures = append(s.failures, container)
		return nil, err
	}

	return container, nil
}

func (s *executor) getServiceNames() ([]string, error) {
	services := s.Config.Docker.Services

	for _, service := range s.options.Services {
		service = s.Build.GetAllVariables().ExpandValue(service)
		err := s.verifyAllowedImage(service, "services", s.Config.Docker.AllowedServices, s.Config.Docker.Services)
		if err != nil {
			return nil, err
		}

		services = append(services, service)
	}

	return services, nil
}

func (s *executor) waitForServices() {
	waitForServicesTimeout := s.Config.Docker.WaitForServicesTimeout
	if waitForServicesTimeout == 0 {
		waitForServicesTimeout = common.DefaultWaitForServicesTimeout
	}

	// wait for all services to came up
	if waitForServicesTimeout > 0 && len(s.services) > 0 {
		s.Println("Waiting for services to be up and running...")
		wg := sync.WaitGroup{}
		for _, service := range s.services {
			wg.Add(1)
			go func(service *docker.Container) {
				s.waitForServiceContainer(service, time.Duration(waitForServicesTimeout)*time.Second)
				wg.Done()
			}(service)
		}
		wg.Wait()
	}
}

func (s *executor) buildServiceLinks(linksMap map[string]*docker.Container) (links []string) {
	for linkName, container := range linksMap {
		newContainer, err := s.client.InspectContainer(container.ID)
		if err != nil {
			continue
		}
		if newContainer.State.Running {
			links = append(links, container.ID+":"+linkName)
		}
	}
	return
}

func (s *executor) createFromServiceDescription(description string, linksMap map[string]*docker.Container) (err error) {
	var container *docker.Container

	service, version, imageName, linkNames := s.splitServiceAndVersion(description)

	for _, linkName := range linkNames {
		if linksMap[linkName] != nil {
			s.Warningln("Service", description, "is already created. Ignoring.")
			continue
		}

		// Create service if not yet created
		if container == nil {
			container, err = s.createService(service, version, imageName)
			if err != nil {
				return
			}
			s.Debugln("Created service", description, "as", container.ID)
			s.services = append(s.services, container)
		}
		linksMap[linkName] = container
	}
	return
}

func (s *executor) createServices() (err error) {
	serviceNames, err := s.getServiceNames()
	if err != nil {
		return
	}

	linksMap := make(map[string]*docker.Container)

	for _, serviceDescription := range serviceNames {
		err = s.createFromServiceDescription(serviceDescription, linksMap)
		if err != nil {
			return
		}
	}

	s.waitForServices()

	s.links = s.buildServiceLinks(linksMap)
	return
}

func (s *executor) createContainer(containerType, imageName string, cmd []string) (container *docker.Container, err error) {
	// Fetch image
	image, err := s.getDockerImage(imageName)
	if err != nil {
		return nil, err
	}

	hostname := s.Config.Docker.Hostname
	if hostname == "" {
		hostname = s.Build.ProjectUniqueName()
	}

	containerName := s.Build.ProjectUniqueName() + "-" + containerType

	options := docker.CreateContainerOptions{
		Name: containerName,
		Config: &docker.Config{
			Image:        image.ID,
			Hostname:     hostname,
			Cmd:          cmd,
			Labels:       s.getLabels(containerType),
			Tty:          false,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    true,
			Env:          append(s.Build.GetAllVariables().StringList(), s.BuildShell.Environment...),
		},
		HostConfig: &docker.HostConfig{
			CPUSetCPUs:    s.Config.Docker.CPUSetCPUs,
			DNS:           s.Config.Docker.DNS,
			DNSSearch:     s.Config.Docker.DNSSearch,
			Privileged:    s.Config.Docker.Privileged,
			CapAdd:        s.Config.Docker.CapAdd,
			CapDrop:       s.Config.Docker.CapDrop,
			SecurityOpt:   s.Config.Docker.SecurityOpt,
			RestartPolicy: docker.NeverRestart(),
			ExtraHosts:    s.Config.Docker.ExtraHosts,
			NetworkMode:   s.Config.Docker.NetworkMode,
			Links:         append(s.Config.Docker.Links, s.links...),
			Devices:       s.devices,
			Binds:         s.binds,
			VolumesFrom:   append(s.Config.Docker.VolumesFrom, s.volumesFrom...),
			LogConfig: docker.LogConfig{
				Type: "json-file",
			},
		},
	}

	// this will fail potentially some builds if there's name collision
	s.removeContainer(containerName)

	s.Debugln("Creating container", options.Name, "...")
	container, err = s.client.CreateContainer(options)
	if err != nil {
		if container != nil {
			s.failures = append(s.failures, container)
		}
		return nil, err
	}

	s.builds = append(s.builds, container)
	return
}

func (s *executor) killContainer(container *docker.Container, waitCh chan error) (err error) {
	for {
		s.disconnectNetwork(container.ID)
		s.Debugln("Killing container", container.ID, "...")
		s.client.KillContainer(docker.KillContainerOptions{
			ID: container.ID,
		})

		// Wait for signal that container were killed
		// or retry after some time
		select {
		case err = <-waitCh:
			return

		case <-time.After(time.Second):
		}
	}
}

func (s *executor) watchContainer(container *docker.Container, input io.Reader, abort chan interface{}) (err error) {
	s.Debugln("Starting container", container.ID, "...")
	err = s.client.StartContainer(container.ID, nil)
	if err != nil {
		return
	}

	options := docker.AttachToContainerOptions{
		Container:    container.ID,
		InputStream:  input,
		OutputStream: s.BuildTrace,
		ErrorStream:  s.BuildTrace,
		Logs:         false,
		Stream:       true,
		Stdin:        true,
		Stdout:       true,
		Stderr:       true,
		RawTerminal:  false,
	}

	waitCh := make(chan error, 1)
	go func() {
		s.Debugln("Attaching to container", container.ID, "...")
		err = s.client.AttachToContainer(options)
		if err != nil {
			waitCh <- err
			return
		}

		s.Debugln("Waiting for container", container.ID, "...")
		exitCode, err := s.client.WaitContainer(container.ID)
		if err == nil {
			if exitCode != 0 {
				err = &common.BuildError{
					Inner: fmt.Errorf("exit code %d", exitCode),
				}
			}
		}
		waitCh <- err
	}()

	select {
	case <-abort:
		s.killContainer(container, waitCh)
		err = errors.New("Aborted")

	case err = <-waitCh:
		s.Debugln("Container", container.ID, "finished with", err)
	}
	return
}

func (s *executor) removeContainer(id string) error {
	s.disconnectNetwork(id)
	removeContainerOptions := docker.RemoveContainerOptions{
		ID:            id,
		RemoveVolumes: true,
		Force:         true,
	}
	err := s.client.RemoveContainer(removeContainerOptions)
	s.Debugln("Removed container", id, "with", err)
	return err
}

func (s *executor) disconnectNetwork(id string) error {
	netList, err := s.client.ListNetworks()
	if err != nil {
		s.Debugln("Can't get network list. ListNetworks exited with", err)
		return err
	}
	disconnectNetworkOptions := docker.NetworkConnectionOptions{
		Container: id,
	}
	for _, network := range netList {
		for _, pluggedContainer := range network.Containers {
			if id == pluggedContainer.Name {
				networkID := network.ID
				err = s.client.DisconnectNetwork(networkID, disconnectNetworkOptions)
				if err != nil {
					s.Warningln("Can't disconnect possibly zombie container", pluggedContainer.Name, "from network", network.Name, "->", err)
				} else {
					s.Warningln("Possibly zombie container", pluggedContainer.Name, "is disconnected from network", network.Name)
				}
				break
			}
		}
	}
	return err
}

func (s *executor) verifyAllowedImage(image, optionName string, allowedImages []string, internalImages []string) error {
	for _, allowedImage := range allowedImages {
		ok, _ := filepath.Match(allowedImage, image)
		if ok {
			return nil
		}
	}

	for _, internalImage := range internalImages {
		if internalImage == image {
			return nil
		}
	}

	if len(allowedImages) != 0 {
		s.Println()
		s.Errorln("The", image, "is not present on list of allowed", optionName)
		for _, allowedImage := range allowedImages {
			s.Println("-", allowedImage)
		}
		s.Println()
	} else {
		// by default allow to override the image name
		return nil
	}

	s.Println("Please check runner's configuration: http://doc.gitlab.com/ci/docker/using_docker_images.html#overwrite-image-and-services")
	return errors.New("invalid image")
}

func (s *executor) getImageName() (string, error) {
	if s.options.Image != "" {
		image := s.Build.GetAllVariables().ExpandValue(s.options.Image)
		err := s.verifyAllowedImage(s.options.Image, "images", s.Config.Docker.AllowedImages, []string{s.Config.Docker.Image})
		if err != nil {
			return "", err
		}
		return image, nil
	}

	if s.Config.Docker.Image == "" {
		return "", errors.New("No Docker image specified to run the build in")
	}

	return s.Config.Docker.Image, nil
}

func (s *executor) connectDocker() (err error) {
	client, err := docker_helpers.New(s.Config.Docker.DockerCredentials, DockerAPIVersion)
	if err != nil {
		return err
	}
	s.client = client

	s.info, err = client.Info()
	if err != nil {
		return err
	}

	return
}

func (s *executor) createDependencies() (err error) {
	err = s.bindDevices()
	if err != nil {
		return err
	}

	s.Debugln("Creating build volume...")
	err = s.createBuildVolume()
	if err != nil {
		return err
	}

	s.Debugln("Creating services...")
	err = s.createServices()
	if err != nil {
		return err
	}

	s.Debugln("Creating user-defined volumes...")
	err = s.createUserVolumes()
	if err != nil {
		return err
	}

	return
}

func (s *executor) Prepare(globalConfig *common.Config, config *common.RunnerConfig, build *common.Build) error {
	err := s.prepareBuildsDir(config)
	if err != nil {
		return err
	}

	err = s.AbstractExecutor.Prepare(globalConfig, config, build)
	if err != nil {
		return err
	}

	if s.BuildShell.PassFile {
		return errors.New("Docker doesn't support shells that require script file")
	}

	if config.Docker == nil {
		return errors.New("Missing docker configuration")
	}

	err = build.Options.Decode(&s.options)
	if err != nil {
		return err
	}

	imageName, err := s.getImageName()
	if err != nil {
		return err
	}

	s.Println("Using Docker executor with image", imageName, "...")

	err = s.connectDocker()
	if err != nil {
		return err
	}

	err = s.createDependencies()
	if err != nil {
		return err
	}
	return nil
}

func (s *executor) prepareBuildsDir(config *common.RunnerConfig) error {
	rootDir := config.BuildsDir
	if rootDir == "" {
		rootDir = s.DefaultBuildsDir
	}
	if s.isHostMountedVolume(rootDir, config.Docker.Volumes...) {
		s.SharedBuildsDir = true
	}
	return nil
}

func (s *executor) Cleanup() {
	var wg sync.WaitGroup

	remove := func(id string) {
		wg.Add(1)
		go func() {
			s.removeContainer(id)
			wg.Done()
		}()
	}

	for _, failure := range s.failures {
		remove(failure.ID)
	}

	for _, service := range s.services {
		remove(service.ID)
	}

	for _, cache := range s.caches {
		remove(cache.ID)
	}

	for _, build := range s.builds {
		remove(build.ID)
	}

	wg.Wait()

	if s.client != nil {
		docker_helpers.Close(s.client)
	}

	s.AbstractExecutor.Cleanup()
}

func (s *executor) runServiceHealthCheckContainer(container *docker.Container, timeout time.Duration) error {
	waitImage, err := s.getPrebuiltImage()
	if err != nil {
		return err
	}

	waitContainerOpts := docker.CreateContainerOptions{
		Name: container.Name + "-wait-for-service",
		Config: &docker.Config{
			Cmd:    []string{"gitlab-runner-service"},
			Image:  waitImage.ID,
			Labels: s.getLabels("wait", "wait="+container.ID),
		},
		HostConfig: &docker.HostConfig{
			RestartPolicy: docker.NeverRestart(),
			Links:         []string{container.Name + ":" + container.Name},
			NetworkMode:   s.Config.Docker.NetworkMode,
			LogConfig: docker.LogConfig{
				Type: "json-file",
			},
		},
	}
	s.Debugln("Waiting for service container", container.Name, "to be up and running...")
	waitContainer, err := s.client.CreateContainer(waitContainerOpts)
	if err != nil {
		return err
	}
	defer s.removeContainer(waitContainer.ID)
	err = s.client.StartContainer(waitContainer.ID, nil)
	if err != nil {
		return err
	}

	waitResult := make(chan error, 1)
	go func() {
		statusCode, err := s.client.WaitContainer(waitContainer.ID)
		if err == nil && statusCode != 0 {
			err = fmt.Errorf("Status code: %d", statusCode)
		}
		waitResult <- err
	}()

	// these are warnings and they don't make the build fail
	select {
	case err := <-waitResult:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("service %v did timeout", container.Name)
	}
}

func (s *executor) waitForServiceContainer(container *docker.Container, timeout time.Duration) error {
	err := s.runServiceHealthCheckContainer(container, timeout)
	if err == nil {
		return nil
	}

	var buffer bytes.Buffer
	buffer.WriteString("\n")
	buffer.WriteString(helpers.ANSI_YELLOW + "*** WARNING:" + helpers.ANSI_RESET + " Service " + container.Name + " probably didn't start properly.\n")
	buffer.WriteString("\n")
	buffer.WriteString(strings.TrimSpace(err.Error()) + "\n")

	var containerBuffer bytes.Buffer

	err = s.client.Logs(docker.LogsOptions{
		Container:    container.ID,
		OutputStream: &containerBuffer,
		ErrorStream:  &containerBuffer,
		Stdout:       true,
		Stderr:       true,
		Timestamps:   true,
	})
	if err == nil {
		if containerLog := containerBuffer.String(); containerLog != "" {
			buffer.WriteString("\n")
			buffer.WriteString(strings.TrimSpace(containerLog))
			buffer.WriteString("\n")
		}
	} else {
		buffer.WriteString(strings.TrimSpace(err.Error()) + "\n")
	}

	buffer.WriteString("\n")
	buffer.WriteString(helpers.ANSI_YELLOW + "*********" + helpers.ANSI_RESET + "\n")
	buffer.WriteString("\n")
	io.Copy(s.BuildTrace, &buffer)
	return err
}
