package generator

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig"
	"github.com/caddyserver/caddy/v2"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/fsnotify/fsnotify"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/caddyfile"
)

func (g *CaddyfileGenerator) getServiceTemplatedCaddyfile(service *swarm.Service, logsBuffer *bytes.Buffer) (*caddyfile.Container, error) {
	err := setupTemplateDirWatcher()
	if err != nil {
		log.Printf("[INFO]: no template dir to watch %s\n", err)
		// don't exit, we'll try again later..
	}

	funcMap := template.FuncMap{
		"entitytype": func(options ...interface{}) (string, error) {
			return "service", nil
		},
		"upstreams": func(options ...interface{}) (string, error) {
			targets, err := g.getServiceProxyTargets(service, logsBuffer, true)
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
			logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Swarm service upstreams %s\n", transformed))
			return strings.Join(transformed, " "), err
		},
		"matcher": func(options ...interface{}) (string, error) {
			// TODO: only a problem if we need to deal with _1...
			matcher := strings.TrimPrefix(service.Spec.Name, "/")
			logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Swarm service matcher %s\n", matcher))

			return matcher, nil
		},
		"labels": func(options ...interface{}) (map[string]string, error) {
			// TODO: mix in image labels - otherwise we miss things like image based virtual.port..

			// TODO: this is not the only place services have labels :/

			logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Swarm service Labels %s\n", service.Spec.Labels))

			return service.Spec.Labels, nil
		},
		"hostname": func(options ...interface{}) (string, error) {
			// if there is a string param, use it.
			if len(options) == 1 {
				if host, isString := options[0].(string); isString && host != "" {
					return host, nil
				}
			}
			// TODO: how do we deal if we have a full domain name?

			// TODO: from compose, looks like caddy-docker-proxy_maintainence_1 (remove _1?)
			return strings.TrimPrefix(service.Spec.Name, "/"), nil
		},
	}
	return g.getTemplatedCaddyfile(service, funcMap, logsBuffer)
}

func (g *CaddyfileGenerator) getContainerTemplatedCaddyfile(container *types.Container, logsBuffer *bytes.Buffer) (*caddyfile.Container, error) {
	err := setupTemplateDirWatcher()
	if err != nil {
		log.Printf("[INFO]: no template dir to watch %s\n", err)
		// don't exit, we'll try again later..
	}

	funcMap := template.FuncMap{
		"entitytype": func(options ...interface{}) (string, error) {
			return "container", nil
		},
		"upstreams": func(options ...interface{}) (string, error) {
			targets, err := g.getContainerIPAddresses(container, logsBuffer, true)
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
				if host, isString := options[0].(string); isString && host != "" {
					return host, nil
				}
			}
			// TODO: how do we deal if we have a full domain name?

			// TODO: from compose, looks like caddy-docker-proxy_maintainence_1 (remove _1?)
			return strings.TrimPrefix(container.Names[0], "/"), nil
		},
	}
	return g.getTemplatedCaddyfile(container, funcMap, logsBuffer)
}

type tmplData struct {
	name string
	tmpl string
}

var loadedTemplates *template.Template
var newTemplate chan tmplData
var templateDirWatcher *fsnotify.Watcher

// NewTemplate adds a new named template to the parsing queue
func NewTemplate(name, tmpl string) {
	newTemplate <- tmplData{
		name: name,
		tmpl: tmpl,
	}
}

func init() {
	newTemplate = make(chan tmplData, 20)

	err := setupTemplateDirWatcher()
	if err != nil {
		log.Printf("[INFO]: no template dir to watch %s\n", err)
		// don't exit, we'll try again later..
	}

	commonFuncMap := template.FuncMap{
		"http": func() string {
			return "http"
		},
		"https": func() string {
			return "https"
		},
	}
	loadedTemplates = template.New("").Funcs(sprig.TxtFuncMap()).Funcs(commonFuncMap)

}

func (g *CaddyfileGenerator) getTemplatedCaddyfile(data interface{}, funcMap template.FuncMap, logsBuffer *bytes.Buffer) (*caddyfile.Container, error) {
	// TODO: how to deal with _1, _2 etc - multiple routes on one container...

	// TODO: need to extract the funcMap, or abstract it better, as we need to over-ride it to cater for the difference between container and service
	loadedTemplates = loadedTemplates.Funcs(funcMap)

	// Parse any found or updated templates TMPL: prefix is to diferentiate from funcMap / named templates
	for {
		select {
		case tmpl := <-newTemplate:
			log.Printf("[DEBUG] parsing template: %s\n", tmpl.name)
			t := loadedTemplates.New("TMPL:" + tmpl.name)
			_, err := t.Parse(tmpl.tmpl)
			if err != nil {
				log.Printf("[ERROR] problem parsing template(%s): %s\n", tmpl.name, err)
			}
		default:
			// no changed templates found
			goto noTemplates
		}
	}
noTemplates:

	var block caddyfile.Container
	for _, tmpl := range loadedTemplates.Templates() {
		if !strings.HasPrefix(tmpl.Name(), "TMPL:") {
			continue
		}
		var writer bytes.Buffer
		err := loadedTemplates.ExecuteTemplate(&writer, tmpl.Name(), data)
		if err != nil {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] ExecuteTemplate (%v)\n", err))
			continue
		}

		newblock, err := caddyfile.Unmarshal(writer.Bytes())
		if err != nil {
			log.Printf("[ERROR] problem converting template to caddyfile block: %s\n", err)
			continue
		}
		block.Merge(newblock)
	}

	return &block, nil
}

func setupTemplateDirWatcher() error {
	if templateDirWatcher != nil {
		// Already initialised
		return nil
	}
	// watch for templates in "${XDG_CONFIG_HOME}/caddy/docker-proxy/"
	rootDir := filepath.Join(caddy.AppConfigDir(), "docker-proxy")
	cleanRoot := filepath.Clean(rootDir)
	info, err := os.Stat(cleanRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("docker-proxy template dir (%s) is not a Directory", cleanRoot)
	}

	templateDirWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			select {
			case event, ok := <-templateDirWatcher.Events:
				if !ok {
					log.Println("[INFO] Stopping watching for filesystem changes")
					return
				}
				if !strings.HasSuffix(event.Name, ".tmpl") {
					log.Printf("[DEBUG] ignoring non .tmpl file %s\n", event.Name)
					continue
				}

				removeBytes := []byte("## removed " + event.Name + " file\n\n")
				b := removeBytes
				b, err = ioutil.ReadFile(event.Name)
				if err != nil {
					log.Printf("[ERROR] reading %s, will remove from templates: %s\n", event.Name, err)
					b = removeBytes
				}

				NewTemplate(event.Name, string(b))
			case err, ok := <-templateDirWatcher.Errors:
				if !ok {
					log.Println("[INFO] Stopping watching for filesystem changes")
					return
				}
				log.Println("[ERROR] ", err)
			}
		}
	}()

	err = templateDirWatcher.Add(cleanRoot)
	if err != nil {
		log.Printf("[ERROR] watching %s: %s\n", cleanRoot, err)
		log.Fatal(err)
	}
	log.Printf("[INFO]: Watching %s for updates to files ending with .tmpl\n", cleanRoot)

	// Also need to read the existing files
	err = filepath.Walk(cleanRoot, func(path string, info os.FileInfo, e1 error) error {
		if !info.IsDir() && strings.HasSuffix(path, ".tmpl") {
			log.Printf("[DEBUG] found template file: %s\n", path)
			if e1 != nil {
				log.Printf("[ERROR] problem walking dir: %s\n", e1)
				return nil // continue with other files
			}

			b, e2 := ioutil.ReadFile(path)
			if e2 != nil {
				log.Printf("[ERROR] problem reading file (%s): %s\n", path, e1)
				return nil // continue with other files
			}
			NewTemplate(path, string(b))
		}
		return nil
	})
	return nil
}
