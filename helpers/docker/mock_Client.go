package docker_helpers

import "github.com/stretchr/testify/mock"

import "github.com/fsouza/go-dockerclient"

type MockClient struct {
	mock.Mock
}

func (m *MockClient) InspectImage(name string) (*docker.Image, error) {
	ret := m.Called(name)

	var r0 *docker.Image
	if ret.Get(0) != nil {
		r0 = ret.Get(0).(*docker.Image)
	}
	r1 := ret.Error(1)

	return r0, r1
}
func (m *MockClient) PullImage(opts docker.PullImageOptions, auth docker.AuthConfiguration) error {
	ret := m.Called(opts, auth)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) ImportImage(opts docker.ImportImageOptions) error {
	ret := m.Called(opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) CreateContainer(opts docker.CreateContainerOptions) (*docker.Container, error) {
	ret := m.Called(opts)

	var r0 *docker.Container
	if ret.Get(0) != nil {
		r0 = ret.Get(0).(*docker.Container)
	}
	r1 := ret.Error(1)

	return r0, r1
}
func (m *MockClient) StartContainer(id string, hostConfig *docker.HostConfig) error {
	ret := m.Called(id, hostConfig)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) WaitContainer(id string) (int, error) {
	ret := m.Called(id)

	r0 := ret.Get(0).(int)
	r1 := ret.Error(1)

	return r0, r1
}
func (m *MockClient) KillContainer(opts docker.KillContainerOptions) error {
	ret := m.Called(opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) InspectContainer(id string) (*docker.Container, error) {
	ret := m.Called(id)

	var r0 *docker.Container
	if ret.Get(0) != nil {
		r0 = ret.Get(0).(*docker.Container)
	}
	r1 := ret.Error(1)

	return r0, r1
}
func (m *MockClient) AttachToContainer(opts docker.AttachToContainerOptions) error {
	ret := m.Called(opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) AttachToContainerNonBlocking(opts docker.AttachToContainerOptions) (docker.CloseWaiter, error) {
	ret := m.Called(opts)

	var r0 docker.CloseWaiter
	if ret.Get(0) != nil {
		r0 = ret.Get(0).(docker.CloseWaiter)
	}
	r1 := ret.Error(0)

	return r0, r1
}
func (m *MockClient) RemoveContainer(opts docker.RemoveContainerOptions) error {
	ret := m.Called(opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) DisconnectNetwork(id string, opts docker.NetworkConnectionOptions) error {
	ret := m.Called(id, opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) ListNetworks() ([]docker.Network, error) {
	ret := m.Called()

	var r0 []docker.Network
	if ret.Get(0) != nil {
		r0 = ret.Get(0).([]docker.Network)
	}
	r1 := ret.Error(1)

	return r0, r1
}
func (m *MockClient) Logs(opts docker.LogsOptions) error {
	ret := m.Called(opts)

	r0 := ret.Error(0)

	return r0
}
func (m *MockClient) Info() (*docker.Env, error) {
	ret := m.Called()

	var r0 *docker.Env
	if ret.Get(0) != nil {
		r0 = ret.Get(0).(*docker.Env)
	}
	r1 := ret.Error(1)

	return r0, r1
}
