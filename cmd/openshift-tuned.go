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
	"reflect"   // reflect.DeepEqual()
	"strconv"   // strconv
	"strings"   // strings.Join()
	"time"      // time.Sleep()

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Types
type arrayFlags []string

type tunedState struct {
	// label2value map
	nodeLabels map[string]string
	// namespace/podname -> label2value map
	podLabels map[string]map[string]string
	change    struct {
		// did node labels change?
		node bool
		// did pod labels change?
		pod bool
		// did tuned profiles/recommend config change on the filesystem?
		cfg bool
	}
}

// Constants
const (
	resyncPeriodDefault = 60
	// Minimum interval between writing changed node/pod labels for tuned daemon in [s]
	labelDumpInterval      = 5
	PNAME                  = "openshift-tuned"
	tunedActiveProfileFile = "/etc/tuned/active_profile"
	tunedProfilesConfigMap = "/var/lib/tuned/profiles-data/tuned-profiles.yaml"
	tunedProfilesDir       = "/etc/tuned"
)

// Global variables
var (
	boolPullLabels  = flag.Bool("pull", false, "query node/pod labels (pull model)")
	boolWatchLabels = flag.Bool("watch", true, "watch for node/pod label changes (push model)")
	fileNodeLabels  = "/var/lib/tuned/ocp-node-labels.cfg"
	filePodLabels   = "/var/lib/tuned/ocp-pod-labels.cfg"
	fileWatch       arrayFlags
	apiPort         = flag.Int("p", 0, "port to listen on for API requests, 0 disables the functionality")
)

// Functions
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

	flag.Var(&fileWatch, "watch-file", "Files/directories to watch for changes.")
	flag.StringVar(&fileNodeLabels, "node-labels", fileNodeLabels, "File to dump node-labels to for tuned.")
	flag.StringVar(&filePodLabels, "pod-labels", filePodLabels, "File to dump pod-labels to for tuned.")
	flag.Parse()
}

func profilesExtract() {
	log.Printf("Extracting tuned profiles\n")

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
	log.Printf("Reloading tuned...\n")
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

func nodeLabelsGet(clientset *kubernetes.Clientset, nodeName string) map[string]string {
	node, err := clientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		log.Printf("Node %s not found\n", nodeName)
	} else if statusError, isStatus := err.(*errors.StatusError); isStatus {
		log.Printf("Error getting node %s: %v\n", nodeName, statusError.ErrStatus.Message)
	}
	if err != nil {
		panic(err.Error())
	}

	return node.Labels
}

func podLabelsGet(clientset *kubernetes.Clientset, nodeName string) map[string]map[string]string {
	var sb strings.Builder
	pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		panic(err.Error())
	}

	podLabels := map[string]map[string]string{}
	for _, pod := range pods.Items {
		sb.WriteString(pod.Namespace)
		sb.WriteString("/")
		sb.WriteString(pod.Name)
		podLabels[sb.String()] = pod.Labels // key is "podNamespace/podName"
		sb.Reset()
	}

	return podLabels
}

func nodeLabelsDump(labels map[string]string, fileLabels string) {
	f, err := os.Create(fileLabels)
	if err != nil {
		log.Fatalf("Failed to create labels file '%s': %v", fileLabels, err)
	}
	defer f.Close()

	log.Printf("Dumping labels to %s\n", fileLabels)
	for key, value := range labels {
		_, err := f.WriteString(fmt.Sprintf("%s=%s\n", key, value))
		if err != nil {
			log.Fatalf("Error writing to labels file %s: %v\n", fileLabels, err)
		}
	}
	f.Sync()
}

func podLabelsDump(labels map[string]map[string]string, fileLabels string) {
	f, err := os.Create(fileLabels)
	if err != nil {
		log.Fatalf("Failed to create labels file '%s': %v", fileLabels, err)
	}
	defer f.Close()

	log.Printf("Dumping labels to %s\n", fileLabels)
	for _, values := range labels {
		for key, value := range values {
			_, err := f.WriteString(fmt.Sprintf("%s=%s\n", key, value))
			if err != nil {
				log.Fatalf("Error writing to labels file %s: %v\n", fileLabels, err)
			}
		}
	}
	f.Sync()
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

func nodeWatch(clientset *kubernetes.Clientset, nodeName string) watch.Interface {
	w, err := clientset.CoreV1().Nodes().Watch(metav1.ListOptions{FieldSelector: "metadata.name=" + nodeName})
	if err != nil {
		log.Fatalf("Unexpected error watching node %s: %v\n", nodeName, err)
	}
	return w
}

func podWatch(clientset *kubernetes.Clientset, nodeName string) watch.Interface {
	w, err := clientset.CoreV1().Pods("").Watch(metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		log.Fatalf("Unexpected error watching pods on %s: %v\n", nodeName, err)
	}
	return w
}

func nodeChangeHandler(event watch.Event, tuned *tunedState) {
	node := event.Object.(*corev1.Node)

	if !reflect.DeepEqual(node.Labels, tuned.nodeLabels) {
		// Node labels changed
		tuned.nodeLabels = node.Labels
		tuned.change.node = true
		return
	}

	// Node labels didn't change, event didn't modify labels
}

// Keep things simple and monitor pod-wide label changes only.  However, note that a
// pod-wide label change doesn't necessarily mean tuned daemon has to be reloaded as
// labels of other pods running on the same node may already have the same labels.
func podChangeHandler(event watch.Event, tuned *tunedState) {
	var sb strings.Builder
	pod := event.Object.(*corev1.Pod)
	sb.WriteString(pod.Namespace)
	sb.WriteString("/")
	sb.WriteString(pod.Name)
	key := sb.String()

	if event.Type == watch.Deleted && tuned.podLabels != nil {
		delete(tuned.podLabels, key)
		tuned.change.pod = true
		return
	}

	if !reflect.DeepEqual(pod.Labels, tuned.podLabels[key]) {
		// Pod labels changed
		if tuned.podLabels == nil {
			tuned.podLabels = map[string]map[string]string{}
		}
		tuned.podLabels[key] = pod.Labels
		tuned.change.pod = true
		return
	}

	// Pod labels didn't change, event didn't modify labels
}

func eventWatch(w watch.Interface, f func(watch.Event, *tunedState), tuned *tunedState) {
	defer w.Stop()
	for event := range w.ResultChan() {
		f(event, tuned)
	}
}

func timedTunedReloader(tuned *tunedState) {
	ticker := time.NewTicker(time.Second * time.Duration(labelDumpInterval))

	for range ticker.C {
		// Check pod labels
		var reload bool = false
		if tuned.change.pod {
			// Pod labels changed
			tuned.change.pod = false
			podLabelsDump(tuned.podLabels, filePodLabels)
			reload = true
		}
		// Check node labels
		if tuned.change.node {
			// Node labels changed
			tuned.change.node = false
			nodeLabelsDump(tuned.nodeLabels, fileNodeLabels)
			reload = true
		}
		// Check tuned profiles/recommend file changes
		if tuned.change.cfg {
			tuned.change.cfg = false
			profilesExtract()
			reload = true
		}
		if reload {
			tunedReload()
		}
	}
}

func pullLabels(clientset *kubernetes.Clientset, tuned *tunedState, nodeName string) {
	var (
		err                  error
		resyncPeriodDuration int64 = resyncPeriodDefault
	)

	if os.Getenv("RESYNC_PERIOD") != "" {
		resyncPeriodDuration, err = strconv.ParseInt(os.Getenv("RESYNC_PERIOD"), 10, 64)
		if err != nil {
			log.Printf("Error: cannot parse RESYNC_PERIOD (%s), using %d\n", os.Getenv("RESYNC_PERIOD"), resyncPeriodDefault)
			resyncPeriodDuration = resyncPeriodDefault
		}
	}

	ticker := time.NewTicker(time.Second * time.Duration(resyncPeriodDuration))

	for range ticker.C {
		// Resync period elapsed, force-pull node and pod labels
		nodeLabels := nodeLabelsGet(clientset, nodeName)
		if !reflect.DeepEqual(nodeLabels, tuned.nodeLabels) {
			tuned.nodeLabels = nodeLabels
			tuned.change.node = true
		}
		podLabels := podLabelsGet(clientset, nodeName)
		if !reflect.DeepEqual(podLabels, tuned.podLabels) {
			tuned.podLabels = podLabels
			tuned.change.pod = true
		}
	}
}

func mainLoop(clientset *kubernetes.Clientset, nodeName string) {
	var tuned tunedState

	if *apiPort > 0 {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/active_profile", apiActiveProfile)
			log.Printf("Listening on %d\n", *apiPort)
			log.Fatal(http.ListenAndServe(fmt.Sprintf((":%d"), *apiPort), mux))
		}()
	}

	// Watch for node and pod label changes
	if *boolWatchLabels {
		go eventWatch(nodeWatch(clientset, nodeName), nodeChangeHandler, &tuned)
		go eventWatch(podWatch(clientset, nodeName), podChangeHandler, &tuned)
	}

	// Create a ticker to do a full node/pod labels pull
	if *boolPullLabels {
		go pullLabels(clientset, &tuned, nodeName)
	}

	// Create a ticker to dump node/pod labels, extract new profiles and possibly reload tuned;
	// this also rate-limits reloads to a maximum of labelDumpInterval reloads/s
	go timedTunedReloader(&tuned)

	// Watch for filesystem changes on tuned profiles and recommend.conf file(s)
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
				// Ignore Write and Create events, wait for the removal of the old ConfigMap to trigger reload
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					log.Printf("Remove event on: %s\n", event.Name)
					tuned.change.cfg = true
				}
			case err := <-watcher.Errors:
				log.Printf("Error: %v\n", err)
			}
		}
	}()

	for _, element := range fileWatch {
		watcherAdd(watcher, element)
	}
	<-done
}

func main() {
	var err error

	parseCmdOpts()

	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(1)
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

	mainLoop(clientset, nodeName)
}
