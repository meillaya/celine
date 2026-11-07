package kavita

import "testing"

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
	}
	for _, tc := range cases {
		got, ok := HostPath(tc.runtime, tc.mount, tc.host)
		if ok != tc.ok || got != tc.want {
			t.Errorf("HostPath(%q,%q,%q) = (%q,%v), want (%q,%v)", tc.runtime, tc.mount, tc.host, got, ok, tc.want, tc.ok)
		}
	}
}
