package compose

import (
	"strings"
	"testing"
)

func TestSetServiceImage(t *testing.T) {
	src := `version: '2'
services:
  # the bridge
  homebridge:
    image: homebridge/homebridge:latest   # keep me
    restart: always
  web:
    image: "nginx"
    ports: ["80:80"]
  db:
    image: 'postgres:16'
`
	out, err := SetServiceImage(src, "homebridge", "homebridge/homebridge:2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetServiceImage(out, "web", "nginx:1.31.8")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer("homebridge/homebridge:latest", "homebridge/homebridge:2026-11-02", `"nginx"`, `"nginx:1.31.8"`).Replace(src)
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if img, _ := ServiceImage(out, "db"); img != "postgres:16" {
		t.Errorf("db image %q", img)
	}
	if _, err := SetServiceImage(src, "nope", "x:1"); err == nil {
		t.Error("no error for a missing service")
	}
	if _, err := SetServiceImage("services:\n  a:\n    build: .\n", "a", "x:1"); err == nil {
		t.Error("no error for a service without an image")
	}
}
