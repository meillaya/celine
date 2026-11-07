package tailnet

import (
	"context"
	"strings"
	"testing"

	"github.com/Meillaya/celine/internal/execx"
	"github.com/Meillaya/celine/internal/render"
)

func TestApplyBuildsServeCommands(t *testing.T) {
	runner := &execx.Fake{}
	endpoints := []render.Endpoint{
		{Service: "kavita", HTTPSPort: 443, UpstreamPort: 5000},
		{Service: "calibre", HTTPSPort: 8443, UpstreamPort: 8080},
	}
	if err := Apply(context.Background(), runner, "tailscale", endpoints); err != nil {
		t.Fatal(err)
	}
	lines := runner.CommandLines()
	if len(lines) != 2 {
		t.Fatalf("commands = %d", len(lines))
	}
	if lines[0] != "tailscale serve --bg --yes --https=443 http://127.0.0.1:5000" {
		t.Errorf("kavita command = %q", lines[0])
	}
	if lines[1] != "tailscale serve --bg --yes --https=8443 http://127.0.0.1:8080" {
		t.Errorf("calibre command = %q", lines[1])
	}
}

func TestReadSelfAndHostname(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"BackendState":"Running","CertDomains":["node.tailnet.ts.net"],"Self":{"DNSName":"node.tailnet.ts.net."}}`)}, nil
	}}
	self, err := ReadSelf(context.Background(), runner, "tailscale")
	if err != nil {
		t.Fatal(err)
	}
	if self.DNSName != "node.tailnet.ts.net" {
		t.Fatalf("dns name = %q", self.DNSName)
	}
	if Hostname("", self.DNSName) != "node.tailnet.ts.net" {
		t.Fatalf("hostname = %q", Hostname("", self.DNSName))
	}
	if !self.CertDomainReady("node.tailnet.ts.net") {
		t.Fatalf("cert domain not recognized")
	}
}

func TestClearCommands(t *testing.T) {
	runner := &execx.Fake{}
	endpoints := []render.Endpoint{{Service: "kavita", HTTPSPort: 443, UpstreamPort: 5000}}
	if err := Clear(context.Background(), runner, "tailscale", endpoints); err != nil {
		t.Fatal(err)
	}
	if got := runner.CommandLines()[0]; !strings.Contains(got, "--https=443 off") {
		t.Fatalf("clear command = %q", got)
	}
}
