package generator

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/docker/docker/api/types"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/caddyfile"
)

// TODO: need the same thing that works for services...
func (g *CaddyfileGenerator) getTemplatedCaddyfile(container *types.Container, logsBuffer *bytes.Buffer) (*caddyfile.Container, error) {

	logsBuffer.WriteString(fmt.Sprintf("[INFO] getTemplatedCaddyfile \n"))

	funcMap := template.FuncMap{
		"entitytype": func(options ...interface{}) (string, error) {
			return "container", nil
		},
		"upstreams": func(options ...interface{}) (string, error) {
			targets, err := g.getContainerIPAddresses(container, logsBuffer, true) //getTargets()
			transformed := []string{}
			for _, target := range targets {
				for _, param := range options {
					if protocol, isProtocol := param.(string); isProtocol {
						target = protocol + "://" + target
					} else if port, isPort := param.(int); isPort {
						target = target + ":" + strconv.Itoa(port)
					}
				}
				transformed = append(transformed, target)
			}
			return strings.Join(transformed, " "), err
		},
		"http": func() string {
			return "http"
		},
		"https": func() string {
			return "https"
		},
		"matcher": func(options ...interface{}) (string, error) {
			// TODO: only a problem if we need to deal with _1...
			return strings.TrimPrefix(container.Names[0], "/"), nil
		},
		"labels": func(options ...interface{}) (map[string]string, error) {
			// TODO: mix in image labels...
			return container.Labels, nil
		},
		"hostname": func(options ...interface{}) (string, error) {
			// if there is a string param, use it.
			if len(options) == 1 {
				if host, isString := options[0].(string); isString {
					return host, nil
				}
			}
			// TODO: how do we deal if we have a full domain name?

			// TODO: service name..

			// TODO: from compose, looks like caddy-docker-proxy_maintainence_1 (remove _1?)
			return strings.TrimPrefix(container.Names[0], "/"), nil
		},
	}

	// TODO: how to deal with _1, _2 etc - multiple routes on one container...
	// TODO: virtual.domain... (with default..)
	t, err := template.New("").Funcs(sprig.TxtFuncMap()).Funcs(funcMap).Parse(`
{{ if index labels "virtual.port" }}
*.loc.alho.st loc.alho.st {
			import dns_api_gandi
			@{{matcher}} {
					host {{hostname ((index labels "virtual.host"))}}.loc.alho.st
					{{ if index labels "virtual.path" }}
					path {{ index labels "virtual.path" }}
					{{ end }}
			}
			route @{{matcher}} {
					reverse_proxy {{upstreams ((index labels "virtual.port" | int)) }}
			}
}
{{ end }}
`)
	if err != nil {
		return nil, err
	}
	var writer bytes.Buffer
	err = t.Execute(&writer, container)
	if err != nil {
		return nil, err
	}

	logsBuffer.WriteString(fmt.Sprintf("[INFO] getTemplatedCaddyfile(string) -> (%v)\n", writer.String()))

	//convert to container
	block, err := caddyfile.Unmarshal(writer.Bytes())

	logsBuffer.WriteString(fmt.Sprintf("[INFO] getTemplatedCaddyfile(block) -> (%v)\n", block))

	return block, err

	// return labelsToCaddyfile(container.Labels, container, func() ([]string, error) {
	// 	return g.getContainerIPAddresses(container, logsBuffer, true)
	// })
}
