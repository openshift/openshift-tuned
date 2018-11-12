package main

import (
	"bufio"     // scanner
	"bytes"     // bytes.Buffer
	"flag"      // command-line options parsing
	"fmt"       // Printf()
	"io"        // io.WriteString()
	"io/ioutil" // ioutil.ReadFile()
	"log"       // log.Printf()
	"net/http"  // http server
	"os"        // os.Exit(), os.Signal, os.Stderr, ...
	"os/exec"   // os.Exec()
	"strconv"   // strconv
	"strings"   // strings.Join()
	"time"      // time.Sleep()

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

/* Types */
type arrayFlags []string

/* Constants */
const (
	resyncPeriodDefault    = 60
	PNAME                  = "tuned-wait"
	tunedActiveProfileFile = "/etc/tuned/active_profile"
	tunedProfilesConfigMap = "/var/lib/tuned/profiles-data/tuned-profiles.yaml"
	tunedProfilesDir       = "/etc/tuned"
)

/* Global variables */
var (
	boolDumpNodeLabels = flag.Bool("dump-node-labels", false, "dump node labels and exit")
	fileNodeLabels     = "/var/lib/tuned/ocp-node-labels.cfg"
	fileWatch          arrayFlags
	apiPort            = flag.Int("p", 0, "port to listen on for API requests, 0 disables the functionality")
)

/* Functions */
func mkdir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *arrayFlags) String() string {
	return strings.Join(*a, ",")
}

func (a *arrayFlags) Set(value string) error {
	*a = append(*a, value)
	return nil
}

func parseCmdOpts() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <NODE>\n", PNAME)
		fmt.Fprintf(os.Stderr, "Example: %s b1.lan\n\n", PNAME)
		fmt.Fprintf(os.Stderr, "Options:\n")

		flag.PrintDefaults()
	}

	flag.Var(&fileWatch, "watch", "Files/directories to watch for changes.")
	flag.StringVar(&fileNodeLabels, "l", fileNodeLabels, "File to dump node-labels to for tuned.")
	flag.Parse()
}

func profilesExtract() {
	log.Printf("%s: extracting tuned profiles...\n", PNAME)

	tunedProfilesYaml, err := ioutil.ReadFile(tunedProfilesConfigMap)
	if err != nil {
		log.Fatalf("Failed to open tuned profiles ConfigMap file '%s': %v", tunedProfilesConfigMap, err)
	}

	mProfiles := make(map[string]string)

	err = yaml.Unmarshal(tunedProfilesYaml, &mProfiles)
	if err != nil {
		log.Fatalf("Failed to parse tuned profiles ConfigMap file '%s': %v", tunedProfilesConfigMap, err)
	}

	for key, value := range mProfiles {
		profileDir := fmt.Sprintf("%s/%s", tunedProfilesDir, key)
		profileFile := fmt.Sprintf("%s/%s", profileDir, "tuned.conf")

		err = mkdir(profileDir)
		if err != nil {
			log.Fatalf("Failed to create tuned profile directory '%s': %v", profileDir, err)
		}

		f, err := os.Create(profileFile)
		if err != nil {
			log.Fatalf("Failed to create tuned profile file '%s': %v", profileFile, err)
		}
		defer f.Close()
		_, err = f.WriteString(value)
		if err != nil {
			log.Fatalf("Failed to write tuned profile file '%s': %v", profileFile, err)
		}
	}
}

func tunedReload() {
	log.Printf("%s: reloading tuned...\n", PNAME)
	cmd := exec.Command("/usr/sbin/tuned", "--no-dbus")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	fmt.Fprintf(os.Stderr, "%s", stderr.String()) // do not use log.Printf(), tuned has its own timestamping
	if err != nil {
		panic(err)
	}
}

func nodeLabelsGet(clientset *kubernetes.Clientset, nodeName string) (nodeLabels map[string]string) {
	node, err := clientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	if errors.IsNotFound(err) {
		log.Printf("%s: node %s not found\n", PNAME, nodeName)
	} else if statusError, isStatus := err.(*errors.StatusError); isStatus {
		log.Printf("%s: error getting node %v\n", PNAME, statusError.ErrStatus.Message)
	} else if err != nil {
		panic(err.Error())
	}

	return node.Labels
}

func nodeLabelsRead() map[string]string {
	nodeLabels := make(map[string]string)

	if _, err := os.Stat(fileNodeLabels); os.IsNotExist(err) {
		/* node labels file does not exist */
		return nil
	}

	f, err := os.Open(fileNodeLabels)
	if err != nil {
		log.Fatalf("Error opening node labels file %s: %v\n", fileNodeLabels, err)
	}
	defer f.Close()

	var scanner = bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if equal := strings.Index(line, "="); equal >= 0 {
			if key := strings.TrimSpace(line[:equal]); len(key) > 0 {
				value := line[equal+1:]
				nodeLabels[key] = value
			}
		} else {
			/* no '=' sign found */
			log.Fatalf("Invalid key=value pair in node labels file %s: %s\n", fileNodeLabels, line)
		}
	}

	return nodeLabels
}

func nodeLabelsDump(nodeLabels map[string]string) {
	f, err := os.Create(fileNodeLabels)
	if err != nil {
	}
	defer f.Close()

	for key, value := range nodeLabels {
		_, err := f.WriteString(fmt.Sprintf("%s=%s\n", key, value))
		if err != nil {
			log.Fatalf("Error writing to node labels file %s: %v\n", fileNodeLabels, err)
		}
	}
	f.Sync()
}

func nodeLabelsCompare(nodeLabelsOld map[string]string, nodeLabelsNew map[string]string) bool {
	if nodeLabelsOld == nil {
		/* no node labels defined yet */
		return false
	}
	if len(nodeLabelsOld) != len(nodeLabelsNew) {
		log.Printf("%s: node labels changed\n", PNAME)
		nodeLabelsDump(nodeLabelsNew)
		tunedReload()
		return false
	}
	for key, value := range nodeLabelsNew {
		if nodeLabelsOld[key] != value {
			log.Printf("%s: node label[%s] == %s (old value: %s)\n", PNAME, key, value, nodeLabelsOld[key])
			nodeLabelsDump(nodeLabelsNew)
			tunedReload()
			return false
		}
	}
	return true
}

func watcherAdd(watcher *fsnotify.Watcher, file string) {
	err := watcher.Add(file)
	if err != nil {
		panic(err.Error)
	}
}

func apiActiveProfile(w http.ResponseWriter, req *http.Request) {
	var responseString = ""

	f, err := os.Open(tunedActiveProfileFile)
	if err != nil {
		log.Printf("Error opening tuned active profile file %s: %v\n", tunedActiveProfileFile, err)
	}
	defer f.Close()

	var scanner = bufio.NewScanner(f)
	for scanner.Scan() {
		responseString = strings.TrimSpace(scanner.Text())
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(responseString)))
	io.WriteString(w, responseString)
}

func mainLoop(clientset *kubernetes.Clientset, nodeName string, resyncPeriodDuration int64) {
	nodeLabelsOld := nodeLabelsRead()

	if *apiPort > 0 {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/active_profile", apiActiveProfile)
			log.Printf("%s: listening on %d\n", PNAME, *apiPort)
			log.Fatal(http.ListenAndServe(fmt.Sprintf((":%d"), *apiPort), mux))
		}()
	}

	ticker := time.NewTicker(time.Second * time.Duration(resyncPeriodDuration))
	go func() {
		for range ticker.C {
			nodeLabels := nodeLabelsGet(clientset, nodeName)
			nodeLabelsCompare(nodeLabelsOld, nodeLabels)
			nodeLabelsOld = nodeLabels
		}
	}()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err.Error())
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				log.Printf("%s: event: %v\n", PNAME, event)
				/* Ignore Write and Create events, wait for the removal of the old ConfigMap */
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					log.Printf("%s: modified file: %s\n", PNAME, event.Name)
					profilesExtract()
					tunedReload()
				}
			case err := <-watcher.Errors:
				log.Printf("%s: error: %v\n", PNAME, err)
			}
		}
	}()

	for _, element := range fileWatch {
		watcherAdd(watcher, element)
	}
	<-done
}

func main() {
	var resyncPeriodDuration int64 = resyncPeriodDefault
	var err error

	parseCmdOpts()

	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(1)
	}

	if os.Getenv("RESYNC_PERIOD") != "" {
		resyncPeriodDuration, err = strconv.ParseInt(os.Getenv("RESYNC_PERIOD"), 10, 64)
		if err != nil {
			log.Printf("%s: error: cannot parse RESYNC_PERIOD (%s), using %d\n", PNAME, os.Getenv("RESYNC_PERIOD"), resyncPeriodDefault)
			resyncPeriodDuration = resyncPeriodDefault
		}
	}

	nodeName := flag.Args()[0]

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	profilesExtract()
	nodeLabelsDump(nodeLabelsGet(clientset, nodeName))
	if *boolDumpNodeLabels {
		os.Exit(0)
	}
	tunedReload()

	mainLoop(clientset, nodeName, resyncPeriodDuration)
}
