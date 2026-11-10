package compose

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceImage is the image a service's image: line gives, as written (no
// variables replaced), or "" when it has none.
func ServiceImage(src, service string) (string, error) {
	n, err := imageNode(src, service)
	if err != nil || n == nil {
		return "", err
	}
	return n.Value, nil
}

// SetServiceImage changes the image of a service in a Compose file, leaving
// the rest of the file as it was written: its comments, quotes and layout.
func SetServiceImage(src, service, image string) (string, error) {
	if strings.ContainsAny(image, "\n\"'#") {
		return "", fmt.Errorf("%q isn't an image", image)
	}
	n, err := imageNode(src, service)
	if err != nil {
		return "", err
	}
	if n == nil {
		return "", fmt.Errorf("service %s has no image", service)
	}
	lines := strings.SplitAfter(src, "\n")
	if n.Line < 1 || n.Line > len(lines) {
		return "", errors.New("can't find the image in the file")
	}
	line := lines[n.Line-1]
	start := n.Column - 1 // bytes: Compose files' indentation is ASCII
	if start < 0 || start >= len(line) {
		return "", errors.New("can't find the image in the file")
	}
	end := start
	var quote byte
	switch line[start] {
	case '"', '\'':
		quote = line[start]
		end = strings.IndexByte(line[start+1:], quote)
		if end < 0 {
			return "", errors.New("can't find the end of the image's quotes")
		}
		end += start + 2
	default:
		for end < len(line) && !strings.ContainsRune(" \t\r\n#", rune(line[end])) {
			end++
		}
	}
	old := strings.Trim(line[start:end], `"'`)
	if old != n.Value {
		return "", fmt.Errorf("the image %q is written in a way hlm can't change", n.Value)
	}
	repl := image
	if quote != 0 {
		repl = string(quote) + image + string(quote)
	}
	lines[n.Line-1] = line[:start] + repl + line[end:]
	return strings.Join(lines, ""), nil
}

// imageNode finds services.<service>.image.
func imageNode(src, service string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("a Compose file is a map with services: at the top")
	}
	services := mapValue(doc.Content[0], "services")
	if services == nil {
		return nil, errors.New("the file has no services")
	}
	svc := mapValue(services, service)
	if svc == nil {
		return nil, fmt.Errorf("the file has no service %s", service)
	}
	img := mapValue(svc, "image")
	if img == nil {
		return nil, nil
	}
	if img.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("service %s's image isn't a string", service)
	}
	return img, nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
