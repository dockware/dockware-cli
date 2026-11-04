package clone

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFailHints(t *testing.T) {
	cases := []struct {
		step, output, want string
	}{
		{StepSSH, "sign_and_send_pubkey: signing failed for ED25519 \"x\" from agent: communication with agent failed\nPermission denied (publickey).", "ssh agent could not sign"},
		{StepSSH, "user@host: Permission denied (publickey,password).", "rejected the ssh login"},
		{StepSSH, "ssh: connect to host 10.0.0.1 port 22: Operation timed out", "not reachable"},
		{StepProbe, "ssh: connect to host 10.0.0.1 port 22: Connection timed out", "--ssh-port"},
		{StepSSH, "ssh: Could not resolve hostname nope: nodename nor servname provided", "does not resolve"},
		{StepShadow, "ERROR 1044 (42000) at line 1: Access denied for user 'c1'@'localhost' to database 'shop_dwc_shadow'", "--strategy stream"},
		{StepDatabase, "ERROR 1045 (28000): Access denied for user 'c1'@'localhost' (using password: YES)", "database credentials"},
		{StepDatabase, "ERROR 2003 (HY000): Can't connect to MySQL server on 'db.internal' (111)", "DATABASE_URL host/port"},
		{StepDocker, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", "start Docker Desktop"},
		{StepDocker, "Bind for 0.0.0.0:8000 failed: port is already allocated", "--port"},
		{StepDocker, "curl: (6) Could not resolve host: nodejs.org\nnvm: install v22.11.0 failed!", "no network access from the container"},
		{StepDocker, "! WARNING: Version 'v16.0.0' does not exist.\nN/A: version \"v16.0.0\" is not yet installed.\n\nYou need to run `nvm install v16.0.0` to install and use it.", "pass --node <major>"},
		{StepComposer, "Authentication required (packages.shopware.com)", "--composer-auth"},
		{StepComposer, "Failed to download shopware/core from dist: curl error 22 while downloading https://packages.shopware.com/x.zip: The requested URL returned error: 401 Unauthorized", "--composer-auth"},
		{StepComposer, "curl error 28 while downloading https://repo.packagist.org/packages.json: Connection timed out after 10001 milliseconds", "no network access from the container"},
		{StepComposer, "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "private git package"},
		{StepComposer, "sh: 1: git: not found", "a required tool is missing"},
		{StepRsync, "rsync: [sender] opendir \"/x/files\" failed: Permission denied (13)\nrsync error: some files/attrs were not transferred (code 23)", "cannot read some project files"},
		{StepRsync, "bash: rsync: command not found\nrsync: connection unexpectedly closed (0 bytes received so far) [Receiver]\nrsync error: error in rsync protocol data stream (code 12)", "rsync is missing on the server"},
		{StepUpload, "sh: /tmp/dwc.x/shopware-cli: Permission denied", "noexec"},
		{StepImport, "ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/run/mysqld/mysqld.sock' (2)", "docker compose logs shop"},
		{StepPostImport, "SQLSTATE[HY000] [2002] Connection refused", "docker compose logs shop"},
		{StepVerify, "local mysql: exit status 1: ERROR 2003 (HY000): Can't connect to MySQL server on '127.0.0.1' (111)", "docker compose logs shop"},
		{StepImport, "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)", "stock dockware image"},
		{StepImport, "ERROR 1064 (42000) at line 12: You have an error in your SQL syntax", "dump does not import"},
	}
	for _, c := range cases {
		err := Fail(c.step, "x", errors.New("exit status 1"), c.output)
		var ce *Error
		if !errors.As(err, &ce) || !strings.Contains(ce.Hint, c.want) {
			t.Errorf("%s %q: hint %q, want it to contain %q", c.step, c.output, ce.Hint, c.want)
		}
	}
}

func TestFailKeepsInnerClassification(t *testing.T) {
	inner := Fail(StepSSH, "ssh failed", errors.New("exit status 255"), "Permission denied (publickey).")
	outer := Fail(StepProbe, "probe failed", fmt.Errorf("probe: %w", inner), "")
	var ce *Error
	if !errors.As(outer, &ce) || ce.Step != StepSSH {
		t.Fatalf("outer classification replaced the inner one: %v", outer)
	}
}

func TestHintsAreStepScoped(t *testing.T) {
	// a generic "Permission denied" during the import is not an rsync or upload problem
	if h := Fail(StepImport, "import", nil, "Permission denied").(*Error).Hint; h != "" {
		t.Fatalf("unexpected hint %q", h)
	}
	// failures inside the container must not send the user to the server
	cases := []struct {
		step, output, forbidden string
	}{
		{StepComposer, "curl error 28 while downloading https://repo.packagist.org/packages.json: Connection timed out after 10001 milliseconds", "--ssh-port"},
		{StepDocker, "curl: (6) Could not resolve host: nodejs.org", "ssh"},
		{StepImport, "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)", ".env"},
		{StepPostImport, "SQLSTATE[HY000] [2002] Connection refused", "ssh host"},
		{StepVerify, "ERROR 2002 (HY000): Can't connect to local MySQL server through socket", "ssh host"},
		{StepComposer, "sh: 1: git: not found", "rsync"},
	}
	for _, c := range cases {
		if h := Fail(c.step, "x", nil, c.output).(*Error).Hint; strings.Contains(h, c.forbidden) {
			t.Errorf("%s %q: hint %q points at the server (%q)", c.step, c.output, h, c.forbidden)
		}
	}
}

func TestSSHClientFailedTellsSSHFromRemoteExit255(t *testing.T) {
	client := []string{
		"ssh: connect to host 10.0.0.1 port 22: Operation timed out",
		"ssh: Could not resolve hostname nope: nodename nor servname provided, or not known",
		"user@host: Permission denied (publickey,password).",
		"Load key \"/Users/me/.ssh/id\": Permission denied\nuser@host: Permission denied (publickey).",
		"sign_and_send_pubkey: signing failed for ED25519 \"x\" from agent: agent refused operation\nuser@host: Permission denied (publickey).",
		"Host key verification failed.",
		"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@",
		"kex_exchange_identification: Connection closed by remote host\nConnection closed by 10.0.0.1 port 22",
		"ssh_exchange_identification: read: Connection reset by peer",
		"Connection reset by 10.0.0.1 port 22",
		"banner exchange: Connection to 10.0.0.1 port 22: Connection timed out",
		"Connection timed out during banner exchange",
		"client_loop: send disconnect: Broken pipe",
		"packet_write_wait: Connection to 10.0.0.1 port 22: Broken pipe",
		"Received disconnect from 10.0.0.1 port 22:2: Too many authentication failures",
		"Timeout, server host not responding.",
		"Unable to negotiate with 10.0.0.1 port 22: no matching host key type found. Their offer: ssh-rsa",
		"Bad owner or permissions on /Users/me/.ssh/config",
		"command-line: line 0: Bad configuration option: nope",
	}
	for _, s := range client {
		if !sshClientFailed(s) {
			t.Errorf("ssh client message not recognised: %q", s)
		}
	}
	remote := []string{
		"",
		"PHP Fatal error:  Uncaught Error: Call to undefined function foo() in Command line code:1\nStack trace:\n#0 {main}\n  thrown in Command line code on line 1",
		"PHP Parse error:  syntax error, unexpected end of file in /var/www/x/.env.php on line 3",
		"ERROR 1045 (28000): Access denied for user 'c1'@'localhost' (using password: YES)",
		"ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/run/mysqld/mysqld.sock' (2)",
		"sh: 1: mysqldump: not found",
		"Warning: Permanently added 'host' (ED25519) to the list of known hosts.\nPHP Fatal error:  Allowed memory size exhausted",
		"cp: cannot open '/var/www/x/.env' for reading: Permission denied",
	}
	for _, s := range remote {
		if sshClientFailed(s) {
			t.Errorf("remote script output taken for an ssh client failure: %q", s)
		}
	}
}
