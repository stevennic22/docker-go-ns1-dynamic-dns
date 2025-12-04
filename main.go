package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/linode/linodego"
	"golang.org/x/oauth2"
	ns1 "gopkg.in/ns1/ns1-go.v2/rest"
	ns1data "gopkg.in/ns1/ns1-go.v2/rest/model/data"
	ns1model "gopkg.in/ns1/ns1-go.v2/rest/model/dns"
)

var configFilePath = "/app/config/config.yml"

// The full imported config
type Config map[string]DomainConfig

// Configuration for an entire domain
type DomainConfig struct {
	Test             bool      `yaml:"test"`
	Debug            bool      `yaml:"debug"`
	Timeout          int       `yaml:"timeout"`
	Delay            int       `yaml:"delay"`
	AllowedCountries []string  `yaml:"allowed_countries"`
	Hosts            []Host    `yaml:"hosts"`
	NS1              NS1Config `yaml:"ns1"`
}

// NS1 config object
type NS1Config struct {
	APIKey string `yaml:"api-key"`
	Client *ns1.Client
}

// Create NS1 client for a domain/Zone
func (n *NS1Config) configureNS1Client(timeout int, debug bool) {
	if timeout < 1 {
		timeout = 30
	}

	httpClient := &http.Client{Timeout: time.Second * time.Duration(timeout)}

	var doer ns1.Doer

	// If debug flag is enabled, add logging to http client
	if debug {
		logger := log.New(os.Stdout, "[NS1] ", log.LstdFlags)
		doer = ns1.Decorate(httpClient, ns1.Logging(logger))
	} else {
		doer = ns1.Decorate(httpClient)
	}
	n.Client = ns1.NewClient(doer, ns1.SetAPIKey(n.APIKey))
}

// An individual Linode host from the config
type Host struct {
	Name       string   `yaml:"name"`
	Method     string   `yaml:"method"`
	APIKey     string   `yaml:"API_KEY,omitempty"`
	HostKey    string   `yaml:"HOST_KEY,omitempty"`
	Subdomains []string `yaml:"subdomains"`
	Client     linodego.Client
}

// Configure the Linode client
func (h *Host) configureLinodeClient(timeout int, debug bool) {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: h.APIKey})

	if timeout < 1 {
		timeout = 30
	}

	oauth2Client := &http.Client{
		Transport: &oauth2.Transport{
			Source: tokenSource,
		},
		Timeout: time.Second * time.Duration(timeout),
	}

	h.Client = linodego.NewClient(oauth2Client)

	// Enable debug logging through Linode client
	h.Client.SetDebug(debug)
}

// Fetches the public IP from Linode API
// If there are multiple, only the first one is returned
func (h *Host) linodeAPICheck() (string, error) {
	log.Println("Checking against: Linode API")
	hostID, _ := strconv.Atoi(h.HostKey)
	res, instanceErr := h.Client.GetInstance(context.Background(), hostID)

	if instanceErr != nil {
		return "", fmt.Errorf("linode API returned error %w", instanceErr)
	}

	ips := res.IPv4
	if len(ips) < 1 {
		return "", fmt.Errorf("no IPv4 addresses on host")
	}

	return ips[0].String(), nil
}

type IPCheckResponse struct {
	IP string `json:"ip"`
}

// Fetches the public IP from a randomly chosen external service
func externalWAN(timeout int) (string, error) {
	resources := []string{
		"https://api.ipify.org?format=json",
		"https://ipinfo.io/json",
		"https://ifconfig.co/json",
	}

	thePick := rand.Intn(len(resources))

	url, _ := url.Parse(resources[thePick])
	log.Printf("Checking against: %s", url.Host)

	client := &http.Client{Timeout: time.Second * time.Duration(timeout)}

	resp, ipCheckErr := client.Get(url.String())
	if ipCheckErr != nil {
		return "", fmt.Errorf("failed to get external IP: %w", ipCheckErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("external service returned status: %d", resp.StatusCode)
	}

	var result IPCheckResponse
	if decoderErr := json.NewDecoder(resp.Body).Decode(&result); decoderErr != nil {
		return "", fmt.Errorf("failed to decode response: %w", decoderErr)
	}

	return strings.TrimSpace(result.IP), nil
}

// Fetches the IP based on the host's configured method
// Current options:
//
//	LinodeAPI
//	- Pulls from Linode API
//	External
//	- Pulls from an IP checking service
func getIP(host Host, timeout int) (string, error) {
	switch host.Method {
	case "LinodeAPI":
		return host.linodeAPICheck()
	case "external":
		return externalWAN(timeout)
	default:
		return "", fmt.Errorf("unknown method: %s", host.Method)
	}
}

// Checks whether current IP matches IP in DNS
func checkIP(myIP string, record *ns1model.Record) (bool, error) {
	// Check for _any_ answers
	if len(record.Answers) == 0 {
		log.Printf("Current IP (%s) does not match DNS record for %s (no answers)", myIP, record.Domain)
		return false, nil
	}

	// Check if the first answer has an IPv4 address
	if len(record.Answers[0].Rdata) == 0 {
		log.Printf("Current IP (%s) does not match DNS record for %s (no rdata)", myIP, record.Domain)
		return false, nil
	}

	remoteIP := record.Answers[0].Rdata[0]

	// Check if the host (myIP) matches the answer IP (remoteIP)
	if myIP != remoteIP {
		log.Printf("Current IP (%s) does not match DNS record for %s (%s)", myIP, record.Domain, remoteIP)
		return false, nil
	}

	log.Printf("Current IP (%s) matches DNS record for %s", myIP, record.Domain)
	return true, nil
}

// Commit changes to record (updated IP, new allowed country list, Up flag)
func updateRecord(client *ns1.Client, record *ns1model.Record, answer *ns1model.Answer) error {
	record.Answers = []*ns1model.Answer{
		answer,
	}

	_, updateErr := client.Records.Update(record)
	if updateErr != nil {
		return fmt.Errorf("failed to update record: %w", updateErr)
	}

	log.Printf("Updated record %s", record.Domain)
	return nil
}

// Creates a new `A` record
func createRecord(client *ns1.Client, zone, subdomain, newIP string, allowedCountries []string) error {
	// Add country/Up metadata
	var data *ns1data.Meta

	if len(allowedCountries) > 0 {
		data = &ns1data.Meta{
			Up:      true,
			Country: allowedCountries,
		}
	} else {
		data = &ns1data.Meta{
			Up: true,
		}
	}

	// Build the answer with metadata
	answer := &ns1model.Answer{
		Rdata: []string{newIP},
		Meta:  data,
	}

	var tags = map[string]string{}
	var blockedTags = []string{}

	// Build record and include answer from above
	record := ns1model.NewRecord(zone, subdomain, "A", tags, blockedTags)
	record.Answers = []*ns1model.Answer{answer}

	_, createErr := client.Records.Create(record)
	if createErr != nil {
		return fmt.Errorf("failed to create record: %w", createErr)
	}

	log.Printf("Created new record (%s) with IP: %s", subdomain, newIP)
	return nil
}

// Convert an interface (i.e. allowed countries) to a []string slice
func sliceFromInterface(i interface{}) ([]string, error) {
	if slice, ok := i.([]interface{}); ok {
		result := make([]string, len(slice))
		for idx, item := range slice {
			str, ok := item.(string)
			if !ok {
				return []string{}, fmt.Errorf("element at index %d is not a string (type: %T)", idx, item)
			}
			result[idx] = str
		}
		return result, nil
	} else {
		return []string{}, fmt.Errorf("dunno how we got here")
	}
}

// Return a full domain name
func fullSubdomain(coreDomain, subdomain string) (fullDomain string) {
	if subdomain == "@" {
		fullDomain = coreDomain
	} else {
		fullDomain = subdomain + "." + coreDomain
	}
	return
}

// Read and parse the YAML configuration file
func loadConfig(path string) (Config, error) {
	data, configFileReadErr := os.ReadFile(path)
	if configFileReadErr != nil {
		return nil, fmt.Errorf("failed to read config file: %w", configFileReadErr)
	}

	var config Config
	if yamlMarshalErr := yaml.Unmarshal(data, &config); yamlMarshalErr != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", yamlMarshalErr)
	}

	return config, nil
}

func getConfig() (theConfig Config) {
	// Check if config file exists
	if _, configPathErr := os.Stat(configFilePath); os.IsNotExist(configPathErr) {
		log.Fatalf("%s does not exist or is not a file, exiting.", configFilePath)
	}

	// Load configuration
	theConfig, configErr := loadConfig(configFilePath)
	if configErr != nil {
		log.Fatalf("Failed to load config: %v", configErr)
	}

	return
}

// Process an answer and return meta information
func processAnswer(answer *ns1model.Answer, countries []string) (bool, []string) {
	var shouldUpdate bool
	var countriesToUse []string
	if answer.Meta == nil {
		// No Answer Meta at all
		shouldUpdate = true
		countriesToUse = countries
	} else {
		// Convert Country Meta to slice
		countryMeta, conversionErr := sliceFromInterface(answer.Meta.Country)
		if conversionErr != nil {
			log.Printf("Failure to convert slice: %v. Using default list", conversionErr)
			shouldUpdate = true
			countriesToUse = countries
		}

		if !slices.Equal(countryMeta, countries) {
			log.Println("Meta 'Allowed Countries' differs")

			shouldUpdate = true
			countriesToUse = countries
		}

		if answer.Meta.Up != true {
			log.Println("Meta 'UP' differs")
			shouldUpdate = true

			countriesToUse = countryMeta
		}
	}

	return shouldUpdate, countriesToUse
}

// Load subdomain information to see if record/answer needs to be created or updated
func processSubdomain(domain, currentIP, zone string, NS1Client *ns1.Client, countries []string, test bool) {
	// Try to load an existing record
	record, _, recordCheckErr := NS1Client.Records.Get(zone, domain, "A")

	if recordCheckErr != nil {
		// Assuming record doesn't exist, try to create it
		if !test {
			recordCreateErr := createRecord(NS1Client, zone, domain, currentIP, countries)
			if recordCreateErr != nil {
				log.Printf("Failed to create record %s: %v", domain, recordCreateErr)
			}

		} else {
			log.Printf("TEST MODE: Would create record %s with IP %s", domain, currentIP)
		}

	} else {
		// Record exists, check if IP update is needed
		ipMatches, ipMatchErr := checkIP(currentIP, record)

		if ipMatchErr != nil {
			log.Printf("Failed to check IP for %s: %v", domain, ipMatchErr)
			return
		}

		// Canary check whether Answer needs to be updated
		var shouldUpdate bool
		var countriesToUse []string

		shouldUpdate, countriesToUse = processAnswer(record.Answers[0], countries)

		// Pre-initialize Answer
		var updatedAnswer *ns1model.Answer

		if !ipMatches || shouldUpdate {
			updatedAnswer = &ns1model.Answer{
				Rdata: []string{currentIP},
				Meta: &ns1data.Meta{
					Up:      true,
					Country: countriesToUse,
				},
			}
			shouldUpdate = true
		}

		if shouldUpdate {
			log.Println("Record shold be updated")
			if !test {
				if len(updatedAnswer.Rdata) == 0 {
					updatedAnswer.Rdata = []string{currentIP}
				}

				updatedRecordErr := updateRecord(NS1Client, record, updatedAnswer)
				if updatedRecordErr != nil {
					log.Printf("Failed to update record %s: %v", domain, updatedRecordErr)
				}
			} else {
				log.Printf("TEST MODE: Would update record %s to IP %s", domain, currentIP)
			}
		}
	}

}

func main() {
	config := getConfig()

	// Process each domain
	for domain, dConf := range config {
		log.Printf("Processing domain: %s", domain)

		// Create NS1 client for this domain
		dConf.NS1.configureNS1Client(dConf.Timeout, dConf.Debug)

		// Set default allowed countries
		if len(dConf.AllowedCountries) == 0 {
			dConf.AllowedCountries = []string{"US", "CA"}
		}

		// Load the Zone
		zone, _, zoneCheckErr := dConf.NS1.Client.Zones.Get(domain, true)
		if zoneCheckErr != nil {
			log.Printf("Failed to load zone %s: %v", domain, zoneCheckErr)
			continue
		}

		log.Printf("Zone: %s", zone.Zone)

		// Process each host machine
		for _, host := range dConf.Hosts {
			log.Printf("Checking subdomains for host: %s", host.Name)

			if host.Method == "LinodeAPI" {
				host.configureLinodeClient(dConf.Timeout, dConf.Debug)
			}

			// Get the current IP for this host
			myIP, ipCheckErr := getIP(host, dConf.Timeout)
			if ipCheckErr != nil {
				log.Printf("Failed to get IP for host %s: %v", host.Name, ipCheckErr)
				continue
			}

			// Process each subdomain
			for _, subdomain := range host.Subdomains {
				// Store full domain name (sub.domain.tld)
				fullDomain := fullSubdomain(domain, subdomain)

				// Process individual subdomain
				processSubdomain(fullDomain, myIP, zone.Zone, dConf.NS1.Client, dConf.AllowedCountries, dConf.Test)

				// Add delay between each subdomain to reduce chances of 429s
				time.Sleep(time.Second * time.Duration(dConf.Delay))

			}

			fmt.Println("")

		}

		fmt.Println("")

	}

	log.Println("DNS update completed")

	fmt.Println("")
}
