package generator

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/caddyfile"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/config"
	"github.com/lucaslorentz/caddy-docker-proxy/plugin/v2/docker"
)

// DefaultLabelPrefix for caddy labels in docker
const DefaultLabelPrefix = "caddy"

const swarmAvailabilityCacheInterval = 1 * time.Minute

// CaddyfileGenerator generates caddyfile from docker configuration
type CaddyfileGenerator struct {
	options              *config.Options
	labelRegex           *regexp.Regexp
	dockerClient         docker.Client
	dockerUtils          docker.Utils
	ingressNetworks      map[string]bool
	swarmIsAvailable     bool
	swarmIsAvailableTime time.Time
}

// CreateGenerator creates a new generator
func CreateGenerator(dockerClient docker.Client, dockerUtils docker.Utils, options *config.Options) *CaddyfileGenerator {
	var labelRegexString = fmt.Sprintf("^%s(_\\d+)?(\\.|$)", options.LabelPrefix)

	return &CaddyfileGenerator{
		options:      options,
		labelRegex:   regexp.MustCompile(labelRegexString),
		dockerClient: dockerClient,
		dockerUtils:  dockerUtils,
	}
}

// GenerateCaddyfile generates a caddy file config from docker metadata
func (g *CaddyfileGenerator) GenerateCaddyfile() ([]byte, string, []string) {
	var caddyfileBuffer bytes.Buffer
	var logsBuffer bytes.Buffer

	if g.ingressNetworks == nil {
		ingressNetworks, err := g.getIngressNetworks()
		if err == nil {
			g.ingressNetworks = ingressNetworks
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	}

	if time.Since(g.swarmIsAvailableTime) > swarmAvailabilityCacheInterval {
		g.checkSwarmAvailability(time.Time.IsZero(g.swarmIsAvailableTime))
		g.swarmIsAvailableTime = time.Now()
	}

	caddyfileBlock := caddyfile.CreateContainer()
	controlledServers := []string{}

	// Add caddyfile from path
	if g.options.CaddyfilePath != "" {
		dat, err := ioutil.ReadFile(g.options.CaddyfilePath)
		if err != nil {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		} else {
			block, err := caddyfile.Unmarshal(dat)
			if err != nil {
				logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			} else {
				caddyfileBlock.Merge(block)
			}
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping default Caddyfile because no path is set\n")
	}

	// Add Caddyfile from swarm configs
	if g.swarmIsAvailable {
		configs, err := g.dockerClient.ConfigList(context.Background(), types.ConfigListOptions{})
		if err == nil {
			for _, config := range configs {
				if _, hasLabel := config.Spec.Labels[g.options.LabelPrefix]; hasLabel {
					fullConfig, _, err := g.dockerClient.ConfigInspectWithRaw(context.Background(), config.ID)
					if err != nil {
						logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
					} else {
						block, err := caddyfile.Unmarshal(fullConfig.Spec.Data)
						if err != nil {
							logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
						} else {
							caddyfileBlock.Merge(block)
						}
					}
				}
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping configs because swarm is not available\n")
	}

	// Add Caddyfile templates from swarm configs
	if g.swarmIsAvailable {
		configs, err := g.dockerClient.ConfigList(context.Background(), types.ConfigListOptions{})
		if err == nil {
			caddyTemplateLabelName := strings.Join([]string{g.options.LabelPrefix, "template"}, ".")
			for _, config := range configs {
				if _, hasLabel := config.Spec.Labels[caddyTemplateLabelName]; hasLabel {
					fullConfig, _, err := g.dockerClient.ConfigInspectWithRaw(context.Background(), config.ID)
					if err != nil {
						logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
					} else {
						NewTemplate(config.Spec.Name, string(fullConfig.Spec.Data))
					}
				}
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping config templates because swarm is not available\n")
	}

	// Add services
	if g.swarmIsAvailable {
		services, err := g.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
		if err == nil {
			for _, service := range services {
				logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Swarm service %s\n", service.Spec.Name))

				if _, isControlledServer := service.Spec.Labels[g.options.ControlledServersLabel]; isControlledServer {
					ips, err := g.getServiceTasksIps(&service, &logsBuffer, false)
					if err != nil {
						logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
					} else {
						for _, ip := range ips {
							if g.options.ControllerNetwork == nil || g.options.ControllerNetwork.Contains(net.ParseIP(ip)) {
								controlledServers = append(controlledServers, ip)
							}
						}
					}
				}

				// caddy. labels based config
				serviceCaddyfile, err := g.getServiceCaddyfile(&service, &logsBuffer)
				if err == nil {
					caddyfileBlock.Merge(serviceCaddyfile)
				} else {
					logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
				}

				// template files based config
				containerTemplateCaddyfile, err := g.getServiceTemplatedCaddyfile(&service, &logsBuffer)
				if err == nil {
					// logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Swarm service caddy template %s\n", containerTemplateCaddyfile.Marshal()))
					caddyfileBlock.Merge(containerTemplateCaddyfile)
				} else {
					logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
				}
			}
		} else {
			logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
		}
	} else {
		logsBuffer.WriteString("[INFO] Skipping services because swarm is not available\n")
	}

	// Add containers
	containers, err := g.dockerClient.ContainerList(context.Background(), types.ContainerListOptions{})
	if err == nil {
		for _, container := range containers {
			if serviceName, isService := container.Labels["com.docker.swarm.service.id"]; isService {
				// lets skip this container
				logsBuffer.WriteString(fmt.Sprintf("[DEBUG] skiping container %s, as its a task in service %s\n", container.Names[0], serviceName))

				continue
			}
			logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Container %s\n", container.Names[0]))

			if _, isControlledServer := container.Labels[g.options.ControlledServersLabel]; isControlledServer {
				ips, err := g.getContainerIPAddresses(&container, &logsBuffer, false)
				if err != nil {
					logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
				} else {
					for _, ip := range ips {
						if g.options.ControllerNetwork == nil || g.options.ControllerNetwork.Contains(net.ParseIP(ip)) {
							controlledServers = append(controlledServers, ip)
						}
					}
				}
			}

			// caddy. labels based config
			containerCaddyfile, err := g.getContainerCaddyfile(&container, &logsBuffer)
			if err == nil {
				caddyfileBlock.Merge(containerCaddyfile)
			} else {
				logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			}

			// template files based config
			containerTemplateCaddyfile, err := g.getContainerTemplatedCaddyfile(&container, &logsBuffer)
			if err == nil {
				// logsBuffer.WriteString(fmt.Sprintf("[DEBUG] Container caddy template %s\n", containerTemplateCaddyfile.Marshal()))

				caddyfileBlock.Merge(containerTemplateCaddyfile)
			} else {
				logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
			}
		}
	} else {
		logsBuffer.WriteString(fmt.Sprintf("[ERROR] %v\n", err.Error()))
	}

	// Write global blocks first
	globalCaddyfile := caddyfile.CreateContainer()
	for _, block := range caddyfileBlock.Children {
		if block.IsGlobalBlock() {
			globalCaddyfile.AddBlock(block)
			caddyfileBlock.Remove(block)
		}
	}
	caddyfileBuffer.Write(globalCaddyfile.Marshal())

	// Write remaining blocks
	caddyfileBuffer.Write(caddyfileBlock.Marshal())

	caddyfileContent := caddyfileBuffer.Bytes()

	if g.options.ProcessCaddyfile {
		processCaddyfileContent, processLogs := caddyfile.Process(caddyfileContent)
		caddyfileContent = processCaddyfileContent
		logsBuffer.Write(processLogs)
	}

	if len(caddyfileContent) == 0 {
		caddyfileContent = []byte("# Empty caddyfile")
	}

	if g.options.Mode&config.Server == config.Server {
		controlledServers = append(controlledServers, "localhost")
	}

	// TODO: make optional
	// TODO: get the file location...
	ioutil.WriteFile("/config/caddy/docker-plugin.caddyfile", caddyfileContent, 0644)

	return caddyfileContent, logsBuffer.String(), controlledServers
}

func (g *CaddyfileGenerator) checkSwarmAvailability(isFirstCheck bool) {
	info, err := g.dockerClient.Info(context.Background())
	if err == nil {
		newSwarmIsAvailable := info.Swarm.LocalNodeState == swarm.LocalNodeStateActive
		if isFirstCheck || newSwarmIsAvailable != g.swarmIsAvailable {
			log.Printf("[INFO] Swarm is available: %v\n", newSwarmIsAvailable)
		}
		g.swarmIsAvailable = newSwarmIsAvailable
	} else {
		log.Printf("[ERROR] Swarm availability check failed: %v\n", err.Error())
		g.swarmIsAvailable = false
	}
}

func (g *CaddyfileGenerator) getIngressNetworks() (map[string]bool, error) {
	ingressNetworks := map[string]bool{}

	if len(g.options.IngressNetworks) > 0 {
		networks, err := g.dockerClient.NetworkList(context.Background(), types.NetworkListOptions{})
		if err != nil {
			return nil, err
		}
		for _, dockerNetwork := range networks {
			if dockerNetwork.Ingress {
				continue
			}
			for _, ingressNetwork := range g.options.IngressNetworks {
				if dockerNetwork.Name == ingressNetwork {
					ingressNetworks[dockerNetwork.ID] = true
				}
			}
		}
	} else {
		containerID, err := g.dockerUtils.GetCurrentContainerID()
		if err != nil {
			return nil, err
		}
		log.Printf("[INFO] Caddy ContainerID: %v\n", containerID)
		container, err := g.dockerClient.ContainerInspect(context.Background(), containerID)
		if err != nil {
			return nil, err
		}

		for _, network := range container.NetworkSettings.Networks {
			networkInfo, err := g.dockerClient.NetworkInspect(context.Background(), network.NetworkID, types.NetworkInspectOptions{})
			if err != nil {
				return nil, err
			}
			if networkInfo.Ingress {
				continue
			}
			ingressNetworks[network.NetworkID] = true
		}
	}

	log.Printf("[INFO] IngressNetworksMap: %v\n", ingressNetworks)

	return ingressNetworks, nil
}

func (g *CaddyfileGenerator) filterLabels(labels map[string]string) map[string]string {
	filteredLabels := map[string]string{}
	for label, value := range labels {
		if g.labelRegex.MatchString(label) {
			log.Printf("[INFO]: label %s: %s\n", label, value)
			filteredLabels[label] = value
		}
	}
	return filteredLabels
}
