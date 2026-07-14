package auth

import "testing"

func TestRegionFromIamStartURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://ssoins-65080ad437a5402d.portal.eu-north-1.app.aws/", "eu-north-1"},
		{"https://ssoins-xxx.portal.eu-central-1.app.aws/start", "eu-central-1"},
		{"https://d-123.awsapps.com/start", ""},
		{"https://view.awsapps.com/start", ""},
		{"not-a-url", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := regionFromIamStartURL(tc.in); got != tc.want {
			t.Fatalf("regionFromIamStartURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
