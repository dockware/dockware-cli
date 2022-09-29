package dockercompose

import (
	"reflect"
	"testing"
)

func TestCreate(t *testing.T) {
	got := Create("3.0")
	want := &DockerCompose{
		Version:       "3.0",
		Services:      map[string]*Service{},
		VolumeDrivers: map[string]VolumeDriver{},
	}
	if reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, wanted %#v", got, want)
	}
}

func TestAddOverwriteService(t *testing.T) {
	got := Create("3.0")
	got.AddOverwriteService("foo", "bar", "baz")

	want := Create("3.0")
	want.Services = map[string]*Service{
		"foo": {
			Image:         "bar",
			ContainerName: "baz",
		},
	}

	if reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, wanted %#v", got, want)
	}
}
