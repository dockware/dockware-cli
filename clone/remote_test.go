package clone

import (
	"strings"
	"testing"
)

func TestSSHArgsShareTheConnectionUnlessTheUserConfiguresIt(t *testing.T) {
	// the user's options come first, as ssh keeps the first value of an option
	r := Remote{Target: "shop", Extra: []string{"-o", "ProxyJump=bastion"}, ControlDir: "/tmp/dwc-ssh-1"}
	want := "-o ProxyJump=bastion -o BatchMode=yes -o ConnectTimeout=60 -o ServerAliveInterval=30 -o ControlMaster=auto -o ControlPath=/tmp/dwc-ssh-1/cm -o ControlPersist=60"
	if got := strings.Join(r.SSHArgs(), " "); got != want {
		t.Fatalf("ssh args\n got %s\nwant %s", got, want)
	}
	for _, opt := range []string{"ControlMaster=no", "controlpath ~/.ssh/cm-%C"} {
		r := Remote{Target: "shop", Extra: []string{"-o", opt}}
		done, err := r.ShareConnection()
		if err != nil {
			t.Fatal(err)
		}
		done()
		if r.ControlDir != "" || strings.Contains(strings.Join(r.SSHArgs(), " "), "ControlMaster=auto") {
			t.Errorf("--ssh-option %q must replace the shared connection: %q", opt, r.SSHArgs())
		}
	}
}
