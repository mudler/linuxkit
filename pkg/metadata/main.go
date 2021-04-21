package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	prv "github.com/davidcassany/linuxkit/pkg/metadata/providers"
	log "github.com/sirupsen/logrus"
)

var (
	defaultLogFormatter = &log.TextFormatter{}
)

// infoFormatter overrides the default format for Info() log events to
// provide an easier to read output
type infoFormatter struct {
}

func (f *infoFormatter) Format(entry *log.Entry) ([]byte, error) {
	if entry.Level == log.InfoLevel {
		return append([]byte(entry.Message), '\n'), nil
	}
	return defaultLogFormatter.Format(entry)
}

// netProviders is a list of Providers offering metadata/userdata over the network
var netProviders []prv.Provider

// cdromProviders is a list of Providers offering metadata/userdata data via CDROM
var cdromProviders []prv.Provider

// fileProviders is a list of Providers offering metadata/userdata in a file on the filesystem
var fileProviders []prv.Provider

func main() {
	log.SetFormatter(new(infoFormatter))
	log.SetLevel(log.InfoLevel)
	flagVerbose := flag.Bool("v", false, "Verbose execution")

	flag.Parse()
	if *flagVerbose {
		// Switch back to the standard formatter
		log.SetFormatter(defaultLogFormatter)
		log.SetLevel(log.DebugLevel)
	}

	providers := []string{"aws", "gcp", "hetzner", "openstack", "scaleway", "vultr", "digitalocean", "packet", "metaldata", "cdrom"}
	args := flag.Args()
	if len(args) > 0 {
		providers = args
	}
	for _, p := range providers {
		switch {
		case p == "aws":
			netProviders = append(netProviders, prv.NewAWS())
		case p == "gcp":
			netProviders = append(netProviders, prv.NewGCP())
		case p == "hetzner":
			netProviders = append(netProviders, prv.NewHetzner())
		case p == "openstack":
			netProviders = append(netProviders, prv.NewOpenstack())
		case p == "packet":
			netProviders = append(netProviders, prv.NewPacket())
		case p == "scaleway":
			netProviders = append(netProviders, prv.NewScaleway())
		case p == "vultr":
			netProviders = append(netProviders, prv.NewVultr())
		case p == "digitalocean":
			netProviders = append(netProviders, prv.NewDigitalOcean())
		case p == "metaldata":
			netProviders = append(netProviders, prv.NewMetalData())
		case p == "cdrom":
			cdromProviders = prv.ListCDROMs()
		case strings.HasPrefix(p, "file="):
			fileProviders = append(fileProviders, prv.FileProvider(p[5:]))
		default:
			log.Fatalf("Unrecognised metadata provider: %s", p)
		}
	}

	if err := os.MkdirAll(prv.ConfigPath, 0755); err != nil {
		log.Fatalf("Could not create %s: %s", prv.ConfigPath, err)
	}

	var p prv.Provider
	var userdata []byte
	var err error
	found := false
	for _, p = range netProviders {
		if p.Probe() {
			log.Printf("%s: Probe succeeded", p)
			userdata, err = p.Extract()
			found = true
			break
		}
	}
	if !found {
		for _, p = range cdromProviders {
			log.Printf("Trying %s", p.String())
			if p.Probe() {
				log.Printf("%s: Probe succeeded", p)
				userdata, err = p.Extract()
				found = true
				break
			}
		}
	}
	if !found {
		for _, p = range fileProviders {
			log.Printf("Trying file %s", p.String())
			if p.Probe() {
				log.Printf("%s: Probe succeeded", p)
				userdata, err = p.Extract()
				found = true
				break
			}
		}
	}

	if !found {
		log.Printf("No metadata/userdata found. Bye")
		return
	}

	if err != nil {
		log.Printf("Error during metadata probe: %s", err)
	}

	err = ioutil.WriteFile(path.Join(prv.ConfigPath, "provider"), []byte(p.String()), 0644)
	if err != nil {
		log.Printf("Error writing metadata provider: %s", err)
	}

	if userdata != nil {
		if err := processUserData(prv.ConfigPath, userdata); err != nil {
			log.Printf("Could not extract user data: %s", err)
		}
	}

	// Handle setting the hostname as a special case. We want to
	// do this early and don't really want another container for it.
	hostname, err := ioutil.ReadFile(path.Join(prv.ConfigPath, prv.Hostname))
	if err == nil {
		err := syscall.Sethostname(hostname)
		if err != nil {
			log.Printf("Failed to set hostname: %s", err)
		} else {
			log.Printf("Set hostname to: %s", string(hostname))
		}
	}
}

// If the userdata is a json file, create a directory/file hierarchy.
// Example:
// {
//    "foobar" : {
//        "foo" : {
//            "perm": "0644",
//            "content": "hello"
//        }
// }
// Will create foobar/foo with mode 0644 and content "hello"
func processUserData(basePath string, data []byte) error {
	// Always write the raw data to a file
	err := ioutil.WriteFile(path.Join(basePath, "userdata"), data, 0644)
	if err != nil {
		log.Printf("Could not write userdata: %s", err)
		return err
	}

	var root ConfigFile
	if err := json.Unmarshal(data, &root); err != nil {
		// Userdata is no JSON, presumably...
		log.Printf("Could not unmarshall userdata: %s", err)
		// This is not an error
		return nil
	}

	for dir, entry := range root {
		writeConfigFiles(path.Join(basePath, dir), entry)
	}
	return nil
}

func writeConfigFiles(target string, current Entry) {
	if isFile(current) {
		filemode, err := parseFileMode(current.Perm, 0644)
		if err != nil {
			log.Printf("Failed to parse permission %+v: %s", current, err)
			return
		}
		if err := ioutil.WriteFile(target, []byte(*current.Content), filemode); err != nil {
			log.Printf("Failed to write %s: %s", target, err)
			return
		}
	} else if isDirectory(current) {
		filemode, err := parseFileMode(current.Perm, 0755)
		if err != nil {
			log.Printf("Failed to parse permission %+v: %s", current, err)
			return
		}
		if err := os.MkdirAll(target, filemode); err != nil {
			log.Printf("Failed to create %s: %s", target, err)
			return
		}
		for dir, entry := range current.Entries {
			writeConfigFiles(path.Join(target, dir), entry)
		}
	} else {
		log.Printf("%s is invalid", target)
	}
}

func isFile(json Entry) bool {
	return json.Content != nil && json.Entries == nil
}

func isDirectory(json Entry) bool {
	return json.Content == nil && json.Entries != nil
}

func parseFileMode(input string, defaultMode os.FileMode) (os.FileMode, error) {
	if input != "" {
		perm, err := strconv.ParseUint(input, 8, 32)
		if err != nil {
			return 0, err
		}
		return os.FileMode(perm), nil
	}
	return defaultMode, nil
}

// ConfigFile represents the configuration file
type ConfigFile map[string]Entry

// Entry represents either a directory or a file
type Entry struct {
	Perm    string           `json:"perm,omitempty"`
	Content *string          `json:"content,omitempty"`
	Entries map[string]Entry `json:"entries,omitempty"`
}
