package svc

import (
	"runtime"
	"strings"
	"testing"
)

// The whole point of the user field: a service installed without one runs as
// root, and the server cannot work as root — the embedded Postgres refuses, and
// the data directory resolves to the wrong home. The failure is invisible until
// a reboot, which is why this is a refusal rather than a warning.
func TestRootIsRecognised(t *testing.T) {
	for _, bad := range []string{"", "root"} {
		if !wouldRunAsRoot(bad) {
			t.Errorf("wouldRunAsRoot(%q) = false — a service installed with this runs as root", bad)
		}
	}
	for _, ok := range []string{"bombers", "taboo"} {
		if wouldRunAsRoot(ok) {
			t.Errorf("wouldRunAsRoot(%q) = true, want false", ok)
		}
	}
}

func TestCheckServiceUserRefusesRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows services pick their own account; the rule itself is covered above")
	}
	err := CheckServiceUser("root")
	if err == nil {
		t.Fatal("CheckServiceUser allowed a service that would run as root")
	}
	// The message has to name the way out, or somebody reads "refusing" and
	// has nowhere to go.
	if !strings.Contains(err.Error(), "--user") {
		t.Errorf("the refusal doesn't say how to fix it: %v", err)
	}
	if err := CheckServiceUser("bombers"); err != nil {
		t.Errorf("CheckServiceUser(\"bombers\") = %v, want nil", err)
	}
}

// SUDO_USER is the account that reached root, which is exactly the one that
// owns the install — so it's the right default and an explicit flag still wins.
func TestServiceUserPrefersTheFlagThenSudo(t *testing.T) {
	t.Setenv("SUDO_USER", "bombers")
	t.Setenv("USER", "root")

	if got := ServiceUser("someone-else"); got != "someone-else" {
		t.Errorf("an explicit --user was ignored: got %q", got)
	}
	if got := ServiceUser(""); got != "bombers" {
		t.Errorf("ServiceUser fell back past SUDO_USER: got %q, want bombers", got)
	}
	if got := ServiceUser("   "); got != "bombers" {
		t.Errorf("a blank --user should fall through to SUDO_USER: got %q", got)
	}
}

// Config carries the user into the registration, which is what becomes `User=`
// in the systemd unit. An empty one is legitimate — Windows, and machines where
// the installing account is the running account.
func TestConfigCarriesTheUser(t *testing.T) {
	if got := Config("bombers").UserName; got != "bombers" {
		t.Errorf("Config lost the user: got %q", got)
	}
	if got := Config("").UserName; got != "" {
		t.Errorf("Config invented a user: got %q", got)
	}
	if Config("bombers").Name != "BombersServer" {
		t.Error("the service name changed — an installed unit would no longer be found")
	}
}
