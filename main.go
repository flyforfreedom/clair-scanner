package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"

	"github.com/coreos/clair/api/v1"
	"github.com/fatih/color"
	cli "github.com/jawher/mow.cli"
)

const (
	tmpPrefix           = "clair-scanner-"
	httpPort            = 9279
	postLayerURI        = "/v1/layers"
	getLayerFeaturesURI = "/v1/layers/%s?vulnerabilities"
)

type vulnerabilityInfo struct {
	vulnerability string
	namespace     string
	severity      string
}

type acceptedVulnerability struct {
	Cve         string
	Description string
}

type vulnerabilitiesWhitelist struct {
	GeneralWhitelist map[string]string
	Images           map[string]map[string]string
}

var (
	whitelist = vulnerabilitiesWhitelist{}
)

func main() {
	app := cli.App("clair-scanner", "Scan local Docker images for vulnerabilities with Clair")

	var (
		whitelistFile = app.StringOpt("w whitelist", "", "Path to the whitelist file")
		clair         = app.StringOpt("c clair", "http://127.0.0.1:6060", "Clair url")
		ip            = app.StringOpt("ip", "localhost", "IP addres where clair-scanner is running on")
		imageName     = app.StringArg("IMAGE", "", "Name of the Docker image to scan")
	)

	app.Before = func() {
		if *whitelistFile != "" {
			whitelist = parseWhitelist(*whitelistFile)
		}
	}

	app.Action = func() {
		log.Print("Start clair-scanner")
		start(*imageName, whitelist, *clair, *ip)
	}
	app.Run(os.Args)
}

func parseWhitelist(whitelistFile string) vulnerabilitiesWhitelist {
	whitelistTmp := vulnerabilitiesWhitelist{}

	whitelistBytes, err := ioutil.ReadFile(whitelistFile)
	if err != nil {
		log.Fatal(err)
	}
	if err = yaml.Unmarshal(whitelistBytes, &whitelistTmp); err != nil {
		log.Fatalf("error: %v", err)
	}
	return whitelistTmp
}

func start(imageName string, whitelist vulnerabilitiesWhitelist, clairURL string, scannerIP string) {
	//Create a temporary folder where the docker image layers are going to be stored
	tmpPath := createTmpPath(tmpPrefix)
	defer os.RemoveAll(tmpPath)

	go listenForSignal(func(s os.Signal) {
		log.Fatalf("Application interupted %v", s)
	})

	saveDockerImage(imageName, tmpPath)
	layerIds := getImageLayerIds(tmpPath)

	//Start a server that can serve Docker image layers to Clair
	server := httpFileServer(tmpPath, strconv.Itoa(httpPort))
	defer server.Shutdown(nil)

	analyzeLayers(layerIds, clairURL, scannerIP)
	vulnerabilities, err := getVulnerabilities(clairURL, layerIds)
	if err != nil {
		log.Fatalf("Analyzing failed: %s", err)
	}
	if err = vulnerabilitiesApproved(imageName, vulnerabilities, whitelist); err != nil {
		log.Fatalf("Image contains unapproved vulnerabilities: %s", err)
	}
}

func vulnerabilitiesApproved(imageName string, vulnerabilities []vulnerabilityInfo, whitelist vulnerabilitiesWhitelist) error {
	var unapproved []string
	imageVulnerabilities := getImageVulnerabilities(imageName, whitelist.Images)

	for i := 0; i < len(vulnerabilities); i++ {
		vulnerability := vulnerabilities[i].vulnerability
		vulnerable := true

		if _, exists := whitelist.GeneralWhitelist[vulnerability]; exists {
			vulnerable = false
		}
		if vulnerable && len(imageVulnerabilities) > 0 {
			if _, exists := imageVulnerabilities[vulnerability]; exists {
				vulnerable = false
			}
		}
		if vulnerable {
			unapproved = append(unapproved, vulnerability)
		}
	}
	if len(unapproved) > 0 {
		return fmt.Errorf("%s", unapproved)
	}
	return nil
}

func getImageVulnerabilities(imageName string, whitelistImageVulnerabilities map[string]map[string]string) map[string]string {
	var imageVulnerabilities map[string]string
	imageWithoutVersion := strings.Split(imageName, ":")
	if val, exists := whitelistImageVulnerabilities[imageWithoutVersion[0]]; exists {
		imageVulnerabilities = val
	}
	return imageVulnerabilities
}

func getVulnerabilities(clairURL string, layerIds []string) ([]vulnerabilityInfo, error) {
	var vulnerabilities = make([]vulnerabilityInfo, 0)
	//Last layer gives you all the vulnerabilities of all layers
	rawVulnerabilities, err := fetchLayerVulnerabilities(clairURL, layerIds[len(layerIds)-1])
	if err != nil {
		return vulnerabilities, err
	}
	if len(rawVulnerabilities.Features) == 0 {
		fmt.Printf("%s No features have been detected in the image. This usually means that the image isn't supported by Clair.\n", color.YellowString("NOTE:"))
		return vulnerabilities, nil
	}

	for _, feature := range rawVulnerabilities.Features {
		if len(feature.Vulnerabilities) > 0 {
			for _, vulnerability := range feature.Vulnerabilities {
				vulnerability := vulnerabilityInfo{vulnerability.Name, vulnerability.NamespaceName, vulnerability.Severity}
				vulnerabilities = append(vulnerabilities, vulnerability)
			}
		}
	}
	return vulnerabilities, nil
}

func fetchLayerVulnerabilities(clairURL string, layerID string) (v1.Layer, error) {
	response, err := http.Get(clairURL + fmt.Sprintf(getLayerFeaturesURI, layerID))
	if err != nil {
		return v1.Layer{}, err
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		body, _ := ioutil.ReadAll(response.Body)
		err := fmt.Errorf("Got response %d with message %s", response.StatusCode, string(body))
		return v1.Layer{}, err
	}

	var apiResponse v1.LayerEnvelope
	if err = json.NewDecoder(response.Body).Decode(&apiResponse); err != nil {
		return v1.Layer{}, err
	} else if apiResponse.Error != nil {
		return v1.Layer{}, errors.New(apiResponse.Error.Message)
	}

	return *apiResponse.Layer, nil
}
