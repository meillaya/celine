package kavita

import (
	"context"
	"strings"
	"testing"

	"github.com/Meillaya/celine/internal/execx"
)

func TestHostPathMapping(t *testing.T) {
	cases := []struct {
		runtime string
		mount   string
		host    string
		want    string
		ok      bool
	}{
		{"/data", "/data", "/media/Kavita", "/media/Kavita", true},
		{"/data/Books/Literature Series", "/data", "/media/Kavita", "/media/Kavita/Books/Literature Series", true},
		{"/datax/Books", "/data", "/media/Kavita", "", false},
		{"/other/Books", "/data", "/media/Kavita", "", false},
		{"/data/../../etc/passwd", "/data", "/media/Kavita", "", false},
		{"/data/Books/../../outside", "/data", "/media/Kavita", "", false},
		{"/data/", "/data", "/media/Kavita", "/media/Kavita", true},
	}
	for _, tc := range cases {
		got, ok := HostPath(tc.runtime, tc.mount, tc.host)
		if ok != tc.ok || got != tc.want {
			t.Errorf("HostPath(%q,%q,%q) = (%q,%v), want (%q,%v)", tc.runtime, tc.mount, tc.host, got, ok, tc.want, tc.ok)
		}
	}
}

func TestQueriesUseBusyTimeout(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		for _, arg := range call.Args {
			if strings.Contains(arg, "PRAGMA integrity_check") {
				return execx.Result{Stdout: []byte("ok\n")}, nil
			}
		}
		return execx.Result{Stdout: []byte(`[]`)}, nil
	}}
	if err := IntegrityCheck(context.Background(), runner, "sqlite3", "/tmp/kavita.db"); err != nil {
		t.Fatal(err)
	}
	line := runner.CommandLines()[0]
	if !strings.Contains(line, "-cmd .timeout 5000") {
		t.Fatalf("integrity check has no busy timeout: %s", line)
	}
	var users []User
	if err := queryJSON(context.Background(), runner, "sqlite3", "/tmp/kavita.db", "SELECT 1", &users); err != nil {
		t.Fatal(err)
	}
	line = runner.CommandLines()[1]
	if !strings.Contains(line, "-readonly") || !strings.Contains(line, "-cmd .timeout 5000") {
		t.Fatalf("inventory query args = %s", line)
	}
}

func TestBackupUsesBusyTimeoutAndQuotesDestination(t *testing.T) {
	runner := &execx.Fake{}
	destination := "/tmp/with ' quote/kavita.db"
	if err := Backup(context.Background(), runner, "sqlite3", "/tmp/kavita.db", destination); err != nil {
		t.Fatal(err)
	}
	line := runner.CommandLines()[0]
	if !strings.Contains(line, "-cmd .timeout 5000") {
		t.Fatalf("backup has no busy timeout: %s", line)
	}
	if !strings.Contains(line, ".backup '/tmp/with '' quote/kavita.db'") {
		t.Fatalf("backup command did not quote the destination: %s", line)
	}
}
