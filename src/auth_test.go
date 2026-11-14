package main

import "testing"

func testUsers() Users {
	return Users{
		"tailcat": {Password: "pw1", Mode: ModeMatchedOnly},
		"all":     {Password: "pw2", Mode: ModeAny},
	}
}

func TestAuthenticate(t *testing.T) {
	u := testUsers()
	tests := []struct {
		name, user, pass string
		want             bool
	}{
		{"tailcat ok", "tailcat", "pw1", true},
		{"all ok", "all", "pw2", true},
		{"wrong password", "tailcat", "nope", false},
		{"unknown user", "ghost", "pw1", false},
		{"empty password", "tailcat", "", false},
		{"empty user", "", "pw1", false},
		// 密码是区分大小写的：这是 HTTP 代理凭据，不是用户名。
		{"case sensitive password", "tailcat", "PW1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := u.Authenticate(tt.user, tt.pass)
			if ok != tt.want {
				t.Fatalf("Authenticate(%q,%q) ok = %v, want %v", tt.user, tt.pass, ok, tt.want)
			}
			if ok && got.Mode == "" {
				t.Error("authenticated user must carry a Mode")
			}
		})
	}
}

// 一个用户的密码不能登进另一个用户——这是最容易被表实现写错的地方。
func TestAuthenticateDoesNotCrossUsers(t *testing.T) {
	u := testUsers()
	if _, ok := u.Authenticate("all", "pw1"); ok {
		t.Error("all must not accept tailcat's password")
	}
	if _, ok := u.Authenticate("tailcat", "pw2"); ok {
		t.Error("tailcat must not accept all's password")
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		via  string
		want bool
	}{
		{"matched-only allows tunnel", ModeMatchedOnly, ViaTunnel, true},
		{"matched-only rejects direct", ModeMatchedOnly, ViaDirect, false},
		{"any allows tunnel", ModeAny, ViaTunnel, true},
		{"any allows direct", ModeAny, ViaDirect, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.Allows(tt.via); got != tt.want {
				t.Errorf("%s.Allows(%s) = %v, want %v", tt.mode, tt.via, got, tt.want)
			}
		})
	}
}
