package dockercompose

import (
	"fmt"
)
import "gopkg.in/yaml.v3"

type dockerCompose struct {
	Version       string                  `yaml:"version"`
	Services      map[string]*Service     `yaml:"services"`
	VolumeDrivers map[string]VolumeDriver `yaml:"volumes,omitempty"`
}

type VolumeDriver struct {
	Driver string `yaml:"driver"`
}

type Service struct {
	Image         string   `yaml:"image"`
	ContainerName string   `yaml:"container_name"`
	Ports         []string `yaml:"ports,omitempty"`
	Volumes       []string `yaml:"volumes,omitempty"`
	Environment   []string `yaml:"environment,omitempty"`
	Links         []string `yaml:"links,omitempty"`
}

func Create(version string) dockerCompose {
	return dockerCompose{
		Version:       version,
		Services:      map[string]*Service{},
		VolumeDrivers: map[string]VolumeDriver{},
	}
}

func (dc *dockerCompose) AddOverwriteService(serviceName, containerName, image string) *Service {
	dc.Services[serviceName] = &Service{ContainerName: containerName, Image: image}
	return dc.Services[serviceName]
}

func (dc *dockerCompose) AddVolume(volumeName, driver string) {
	dc.VolumeDrivers[volumeName] = VolumeDriver{Driver: driver}
}

func (s *Service) AddVolume(local, inContainer string) {
	s.Volumes = append(s.Volumes, fmt.Sprintf("%s:%s", local, inContainer))
}

func (s *Service) AddPorts(ports map[int]int) {
	for host, container := range ports {
		s.AddPort(host, container)
	}
}

func (s *Service) AddPort(host, container int) {
	s.Ports = append(s.Ports, fmt.Sprintf("%d:%d", host, container))
}

func (s *Service) AddLinks(domains map[string]string) {
	for serviceName, domain := range domains {
		s.AddLink(serviceName, domain)
	}
}

func (s *Service) AddLink(serviceName, domain string) {
	s.Links = append(s.Links, fmt.Sprintf("%s:%s", serviceName, domain))
}

func (s *Service) AddEnv(key, value string) {
	s.Environment = append(s.Environment, fmt.Sprintf("%s=%s", key, value))
}

func (dc *dockerCompose) ToString() (string, error) {
	d, err := yaml.Marshal(&dc)
	if err != nil {
		return "", err
	}
	return string(d), nil
}
