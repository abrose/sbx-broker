package placeholder

import (
	"reflect"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	got := Format("acme", "idp-client-secret")
	if got != "sbxb_acme_idp_client_secret" {
		t.Fatalf("got %q", got)
	}
}

func TestFindOne(t *testing.T) {
	got := Find("x=sbxb_acme_api_key&y=1")
	want := []Ref{{Namespace: "acme", Secret: "api-key", Start: 2, End: 19}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFindTwo(t *testing.T) {
	s := "a=sbxb_acme_k1 b=sbxb_t2_my_key"
	got := Find(s)
	want := []Ref{
		{Namespace: "acme", Secret: "k1", Start: 2, End: 14},
		{Namespace: "t2", Secret: "my-key", Start: 17, End: 31},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFindNeedsBoundaries(t *testing.T) {
	cases := []string{
		"Xsbxb_acme_k1",
		"sbxb_acme_k1_",
		"sbxb_acme_k1X",
		"sbxb_ACME_k1",
		"sbxb_acme",
		"sbxb__k1",
		"sbxb_" + strings.Repeat("a", 17) + "_k1",
	}
	for _, s := range cases {
		if got := Find(s); len(got) != 0 {
			t.Errorf("Find(%q) = %+v, want none", s, got)
		}
	}
}

func TestFindAcceptsPunctuation(t *testing.T) {
	cases := []string{
		"Bearer sbxb_acme_k1",
		"\"sbxb_acme_k1\"",
		"user:sbxb_acme_k1",
		"/a/sbxb_acme_k1/b",
	}
	for _, s := range cases {
		got := Find(s)
		if len(got) != 1 || got[0].Namespace != "acme" || got[0].Secret != "k1" {
			t.Errorf("Find(%q) = %+v", s, got)
			continue
		}
		if s[got[0].Start:got[0].End] != "sbxb_acme_k1" {
			t.Errorf("Find(%q) offsets cut %q", s, s[got[0].Start:got[0].End])
		}
	}
}

func TestValidNames(t *testing.T) {
	ns := map[string]bool{
		"acme": true, "t1": true, strings.Repeat("a", 16): true,
		"a-b": false, "a_b": false, "": false, strings.Repeat("a", 17): false,
	}
	for in, want := range ns {
		if got := ValidNamespace(in); got != want {
			t.Errorf("ValidNamespace(%q) = %v, want %v", in, got, want)
		}
	}
	secrets := map[string]bool{
		"api-key": true, "k1": true, strings.Repeat("a", 41): true,
		"-a": false, "a_b": false, "A": false, strings.Repeat("a", 42): false,
		"a--b": false, "ab-": false, "": false,
	}
	for in, want := range secrets {
		if got := ValidSecretName(in); got != want {
			t.Errorf("ValidSecretName(%q) = %v, want %v", in, got, want)
		}
	}
	aliases := map[string]bool{
		"api": true, "db-1": true, strings.Repeat("a", 31): true,
		"-a": false, "a_b": false, "": false, strings.Repeat("a", 32): false,
	}
	for in, want := range aliases {
		if got := ValidAlias(in); got != want {
			t.Errorf("ValidAlias(%q) = %v, want %v", in, got, want)
		}
	}
}
