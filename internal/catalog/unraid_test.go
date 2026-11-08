package catalog

import (
	"strings"
	"testing"
)

const unraidXMLTest = `<?xml version="1.0"?>
<Container version="2">
  <Name>Enshrouded</Name>
  <Repository>indifferentbroccoli/enshrouded-server-docker:wine</Repository>
  <Network>br0</Network>
  <MyIP>192.168.1.60</MyIP>
  <Privileged>true</Privileged>
  <TemplateURL>https://raw.githubusercontent.com/juddisjudd/unraid-templates/main/templates/enshrouded-server.xml</TemplateURL>
  <ExtraParams>--restart=unless-stopped</ExtraParams>
  <PostArgs/>
  <CPUset>2,3</CPUset>
  <Config Name="Query Port" Target="15637" Default="15637" Mode="udp" Description="" Type="Port" Display="always" Required="true" Mask="false">15640</Config>
  <Config Name="Server Name" Target="SERVER_NAME" Default="" Mode="" Description="" Type="Variable" Display="always" Required="false" Mask="false"/>
  <Config Name="Mine" Target="/backups" Default="" Mode="rw" Description="" Type="Path" Display="always" Required="false" Mask="false">/mnt/user/backups</Config>
</Container>`

func TestParseUnraidTemplate(t *testing.T) {
	ut, err := ParseUnraidTemplate([]byte(unraidXMLTest))
	if err != nil {
		t.Fatal(err)
	}
	a := ut.App
	if ut.Name != "Enshrouded" || ut.MyIP != "192.168.1.60" || ut.CPUSet != "2,3" || !a.Privileged || a.Network != "br0" || len(a.Config) != 3 {
		t.Fatalf("%+v %+v", ut, a)
	}
	// The same IDs as the feed's, and the user's values (an empty one
	// stays empty).
	if c := a.Config[0]; c.ID != "port:15637/udp" || c.Value != "15640" {
		t.Errorf("port %+v", c)
	}
	if c := a.Config[1]; c.ID != "variable:SERVER_NAME" || c.Value != "" {
		t.Errorf("variable %+v", c)
	}
	if _, err := ParseUnraidTemplate([]byte("<Container></Container>")); err == nil {
		t.Error("no image")
	}
	if _, err := ParseUnraidTemplate([]byte("{}")); err == nil {
		t.Error("not XML")
	}
}

func TestFindTemplate(t *testing.T) {
	c := loadTestCatalog(t)
	url := "https://raw.githubusercontent.com/juddisjudd/unraid-templates/main/templates/enshrouded-server.xml"
	app, tag, ok := c.FindTemplate(url, "anything", "indifferentbroccoli/enshrouded-server-docker:latest")
	if !ok || app.Name != "Enshrouded-broccoli" || tag != "" {
		t.Fatalf("by URL: %v %q %v", app, tag, ok)
	}
	// By its image alone, however it's written.
	if app, _, ok := c.FindTemplate("", "", "docker.io/indifferentbroccoli/enshrouded-server-docker"); !ok || app.Name != "Enshrouded-broccoli" {
		t.Errorf("by image: %v %v", app, ok)
	}
	// Another image isn't that template.
	if _, _, ok := c.FindTemplate(url, "", "someone/else:latest"); ok {
		t.Error("matched another image")
	}
	for _, b := range app.Branches {
		ref := b.Repository
		if ref == "" {
			ref = withTag(app.Repository, b.Tag)
		}
		if _, tag, ok := c.FindTemplate(url, "", ref); !ok || tag != b.Tag {
			t.Errorf("branch %s: %q %v", b.Tag, tag, ok)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	for in, want := range map[string]string{
		"busybox":                        "docker.io/library/busybox:latest",
		"busybox:1.37":                   "docker.io/library/busybox:1.37",
		"index.docker.io/library/nginx":  "docker.io/library/nginx:latest",
		"linuxserver/plex":               "docker.io/linuxserver/plex:latest",
		"lscr.io/linuxserver/plex":       "lscr.io/linuxserver/plex:latest",
		"localhost:5000/x":               "localhost:5000/x:latest",
		"ghcr.io/a/b@sha256:abc":         "ghcr.io/a/b@sha256:abc",
		" LSCR.io/LinuxServer/Plex:Beta": "lscr.io/linuxserver/plex:beta",
	} {
		if got := NormalizeImage(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(NormalizeImage("a/b"), "library") {
		t.Error("library/ for a user's image")
	}
}
