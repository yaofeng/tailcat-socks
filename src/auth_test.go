package main

import (
	"testing"
)

func testUsers() Users {
	return Users{
		"tailcat": {password: "pw1", Mode: ModeMatchedOnly},
		"all":     {password: "pw2", Mode: ModeAny},
	}
}

func TestAuthenticate(t *testing.T) {
	u := testUsers()
	tests := []struct {
		name, user, pass string
		want             bool
		wantMode         Mode
	}{
		{"tailcat ok", "tailcat", "pw1", true, ModeMatchedOnly},
		{"all ok", "all", "pw2", true, ModeAny},
		{"wrong password", "tailcat", "nope", false, ""},
		{"unknown user", "ghost", "pw1", false, ""},
		{"empty password", "tailcat", "", false, ""},
		{"empty user", "", "pw1", false, ""},
		// 密码是区分大小写的：这是 SOCKS5（RFC 1929）用户/密码认证的凭据，
		// 不是 HTTP 代理凭据。
		{"case sensitive password", "tailcat", "PW1", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := u.Authenticate(tt.user, tt.pass)
			if ok != tt.want {
				t.Fatalf("Authenticate(%q,%q) ok = %v, want %v", tt.user, tt.pass, ok, tt.want)
			}
			if got.Mode != tt.wantMode {
				t.Errorf("Authenticate(%q,%q) mode = %q, want %q", tt.user, tt.pass, got.Mode, tt.wantMode)
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

func TestModeForUser(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		want    Mode
		wantErr bool
	}{
		{"tailcat matched-only", "tailcat", ModeMatchedOnly, false},
		{"all unrestricted", "all", ModeAny, false},
		// 未知名字必须报错：拼错会静默放大成直连权限。
		{"capitalized is unknown", "Tailcat", "", true},
		{"typo is unknown", "tailatc", "", true},
		{"empty is unknown", "", "", true},
		{"arbitrary name is unknown", "bob", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := modeForUser(tt.user)
			if (err != nil) != tt.wantErr {
				t.Fatalf("modeForUser(%q) err = %v, wantErr %v", tt.user, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("modeForUser(%q) = %q, want %q", tt.user, got, tt.want)
			}
		})
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

// 未知模式与未知通道都必须拒绝：授权只能显式放行，不能默认放行。
func TestAllowsFailsClosed(t *testing.T) {
	for _, mode := range []Mode{Mode(""), Mode("bogus")} {
		t.Run(string(mode)+" rejects known vias", func(t *testing.T) {
			for _, via := range []string{ViaTunnel, ViaDirect} {
				if mode.Allows(via) {
					t.Errorf("%q.Allows(%s) = true, want false", mode, via)
				}
			}
		})
	}
	t.Run("any rejects unknown via", func(t *testing.T) {
		if ModeAny.Allows("carrier-pigeon") {
			t.Error("ModeAny.Allows(carrier-pigeon) = true, want false")
		}
	})
}
