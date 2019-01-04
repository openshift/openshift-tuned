package main

import (
	"bufio"     // scanner
	"bytes"     // bytes.Buffer
	"flag"      // command-line options parsing
	"fmt"       // Printf()
	"io"        // io.WriteString()
	"io/ioutil" // ioutil.ReadFile()
	"log"       // log.Printf()
	"math/rand" // rand.Seed(), ...
	"net/http"  // http server
	"os"        // os.Exit(), os.Signal, os.Stderr, ...
	"os/exec"   // os.Exec()
	"os/signal" // signal.Notify()
	"reflect"   // reflect.DeepEqual()
	"strconv"   // strconv
	"strings"   // strings.Join()
	"syscall"   // syscall.SIGHUP, ...
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
	sleepRetry          = 5
	// Minimum interval between writing changed node/pod labels for tuned daemon in [s]
	labelDumpInterval      = 5
	PNAME                  = "openshift-tuned"
	tunedActiveProfileFile = "/etc/tuned/active_profile"
	tunedProfilesConfigMap = "/var/lib/tuned/profiles-data/tuned-profiles.yaml"
	tunedProfilesDir       = "/etc/tuned"
)

// Global variables
var (
	done               = make(chan bool, 1)
	terminationSignals = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}
	fileNodeLabels     = "/var/lib/tuned/ocp-node-labels.cfg"
	filePodLabels      = "/var/lib/tuned/ocp-pod-labels.cfg"
	fileWatch          arrayFlags
	version            string // PNAME version
	// Flags
	boolVersion = flag.Bool("version", false, "show program version and exit")
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

func signalHandler() chan os.Signal {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, terminationSignals...)
	go func() {
		sig := <-sigs
		log.Printf("Received signal: %v", sig)
		done <- true
	}()
	return sigs
}

func profilesExtract() error {
	log.Printf("Extracting tuned profiles")

	tunedProfilesYaml, err := ioutil.ReadFile(tunedProfilesConfigMap)
	if err != nil {
		return fmt.Errorf("Failed to open tuned profiles ConfigMap file '%s': %v", tunedProfilesConfigMap, err)
	}

	mProfiles := make(map[string]string)

	err = yaml.Unmarshal(tunedProfilesYaml, &mProfiles)
	if err != nil {
		return fmt.Errorf("Failed to parse tuned profiles ConfigMap file '%s': %v", tunedProfilesConfigMap, err)
	}

	for key, value := range mProfiles {
		profileDir := fmt.Sprintf("%s/%s", tunedProfilesDir, key)
		profileFile := fmt.Sprintf("%s/%s", profileDir, "tuned.conf")

		err = mkdir(profileDir)
		if err != nil {
			return fmt.Errorf("Failed to create tuned profile directory '%s': %v", profileDir, err)
		}

		f, err := os.Create(profileFile)
		if err != nil {
			return fmt.Errorf("Failed to create tuned profile file '%s': %v", profileFile, err)
		}
		defer f.Close()
		_, err = f.WriteString(value)
		if err != nil {
			return fmt.Errorf("Failed to write tuned profile file '%s': %v", profileFile, err)
		}
	}
	return nil
}

func tunedReload() error {
	log.Printf("Reloading tuned...")
	cmd := exec.Command("/usr/sbin/tuned", "--no-dbus")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	fmt.Fprintf(os.Stderr, "%s", stderr.String()) // do not use log.Printf(), tuned has its own timestamping
	return err
}

func nodeLabelsGet(clientset *kubernetes.Clientset, nodeName string) (map[string]string, error) {
	node, err := clientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, fmt.Errorf("Node %s not found", nodeName)
	} else if statusError, isStatus := err.(*errors.StatusError); isStatus {
		return nil, fmt.Errorf("Error getting node %s: %v", nodeName, statusError.ErrStatus.Message)
	}
	if err != nil {
		return nil, err
	}

	return node.Labels, nil
}

func podLabelsGet(clientset *kubernetes.Clientset, nodeName string) (map[string]map[string]string, error) {
	var sb strings.Builder
	pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return nil, err
	}

	podLabels := map[string]map[string]string{}
	for _, pod := range pods.Items {
		sb.WriteString(pod.Namespace)
		sb.WriteString("/")
		sb.WriteString(pod.Name)
		podLabels[sb.String()] = pod.Labels // key is "podNamespace/podName"
		sb.Reset()
	}

	return podLabels, nil
}

func nodeLabelsDump(labels map[string]string, fileLabels string) error {
	f, err := os.Create(fileLabels)
	if err != nil {
		return fmt.Errorf("Failed to create labels file '%s': %v", fileLabels, err)
	}
	defer f.Close()

	log.Printf("Dumping labels to %s", fileLabels)
	for key, value := range labels {
		_, err := f.WriteString(fmt.Sprintf("%s=%s\n", key, value))
		if err != nil {
			return fmt.Errorf("Error writing to labels file %s: %v", fileLabels, err)
		}
	}
	f.Sync()
	return nil
}

func podLabelsDump(labels map[string]map[string]string, fileLabels string) error {
	f, err := os.Create(fileLabels)
	if err != nil {
		return fmt.Errorf("Failed to create labels file '%s': %v", fileLabels, err)
	}
	defer f.Close()

	log.Printf("Dumping labels to %s", fileLabels)
	for _, values := range labels {
		for key, value := range values {
			_, err := f.WriteString(fmt.Sprintf("%s=%s\n", key, value))
			if err != nil {
				return fmt.Errorf("Error writing to labels file %s: %v", fileLabels, err)
			}
		}
	}
	f.Sync()
	return nil
}

func apiActiveProfile(w http.ResponseWriter, req *http.Request) {
	var responseString = ""

	f, err := os.Open(tunedActiveProfileFile)
	if err != nil {
		log.Printf("Error opening tuned active profile file %s: %v", tunedActiveProfileFile, err)
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

func nodeWatch(clientset *kubernetes.Clientset, nodeName string) (watch.Interface, error) {
	w, err := clientset.CoreV1().Nodes().Watch(metav1.ListOptions{FieldSelector: "metadata.name=" + nodeName})
	if err != nil {
		return nil, fmt.Errorf("Unexpected error watching node %s: %v", nodeName, err)
	}
	return w, nil
}

func podWatch(clientset *kubernetes.Clientset, nodeName string) (watch.Interface, error) {
	w, err := clientset.CoreV1().Pods("").Watch(metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return nil, fmt.Errorf("Unexpected error watching pods on %s: %v", nodeName, err)
	}
	return w, nil
}

func nodeChangeHandler(event watch.Event, tuned *tunedState) {
	node, ok := event.Object.(*corev1.Node)
	if !ok {
		log.Printf("Unexpected object received: %#v", event.Object)
		return
	}

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
	pod, ok := event.Object.(*corev1.Pod)
	if !ok {
		log.Printf("Unexpected object received: %#v", event.Object)
		return
	}

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

func timedTunedReloader(tuned *tunedState) (err error) {
	var reload bool

	// Check pod labels
	if tuned.change.pod {
		// Pod labels changed
		tuned.change.pod = false
		if err = podLabelsDump(tuned.podLabels, filePodLabels); err != nil {
			return err
		}
		reload = true
	}
	// Check node labels
	if tuned.change.node {
		// Node labels changed
		tuned.change.node = false
		if err = nodeLabelsDump(tuned.nodeLabels, fileNodeLabels); err != nil {
			return err
		}
		reload = true
	}
	// Check tuned profiles/recommend file changes
	if tuned.change.cfg {
		tuned.change.cfg = false
		if err = profilesExtract(); err != nil {
			return err
		}
		reload = true
	}
	if reload {
		err = tunedReload()
	}
	return err
}

func pullLabels(clientset *kubernetes.Clientset, tuned *tunedState, nodeName string) error {
	// Resync period elapsed, force-pull node and pod labels
	nodeLabels, err := nodeLabelsGet(clientset, nodeName)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(nodeLabels, tuned.nodeLabels) {
		tuned.nodeLabels = nodeLabels
		tuned.change.node = true
	}

	podLabels, err := podLabelsGet(clientset, nodeName)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(podLabels, tuned.podLabels) {
		tuned.podLabels = podLabels
		tuned.change.pod = true
	}
	return nil
}

func pullResyncPeriod() int64 {
	var (
		err                  error
		resyncPeriodDuration int64 = resyncPeriodDefault
	)
	if os.Getenv("RESYNC_PERIOD") != "" {
		resyncPeriodDuration, err = strconv.ParseInt(os.Getenv("RESYNC_PERIOD"), 10, 64)
		if err != nil {
			log.Printf("Error: cannot parse RESYNC_PERIOD (%s), using %d", os.Getenv("RESYNC_PERIOD"), resyncPeriodDefault)
			resyncPeriodDuration = resyncPeriodDefault
		}
	}

	// Add some randomness to the resync period, so that we don't end up in a lockstep querying the API server
	resyncPeriodDuration += rand.Int63n(resyncPeriodDuration/5+1) - resyncPeriodDuration/10
	return resyncPeriodDuration
}

func changeWatcher() (err error) {
	var (
		tuned tunedState
		wPod  watch.Interface
	)

	nodeName := flag.Args()[0]

	err = profilesExtract()
	if err != nil {
		return err
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	// Create a ticker to do a full node/pod labels pull
	resyncPeriod := pullResyncPeriod()
	tickerPull := time.NewTicker(time.Second * time.Duration(resyncPeriod))
	defer tickerPull.Stop()
	log.Printf("Resync period to pull node/pod labels: %d [s]", resyncPeriod)

	// Pull node and pod labels before entering the loop; node labels would be fetched after resyncPeriod
	if err := pullLabels(clientset, &tuned, nodeName); err != nil {
		return err
	}

	// Create a ticker to dump node/pod labels, extract new profiles and possibly reload tuned;
	// this also rate-limits reloads to a maximum of labelDumpInterval reloads/s
	tickerReload := time.NewTicker(time.Second * time.Duration(labelDumpInterval))
	defer tickerReload.Stop()

	if wPod, err = podWatch(clientset, nodeName); err != nil {
		return err
	}
	defer wPod.Stop()

	// Watch for filesystem changes on tuned profiles and recommend.conf file(s)
	wFs, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer wFs.Close()

	// Register fsnotify watchers
	for _, element := range fileWatch {
		err = wFs.Add(element)
		if err != nil {
			return err
		}
	}

	for {
		select {
		case <-done:
			return nil

		case fsEvent := <-wFs.Events:
			// Ignore Write and Create events, wait for the removal of the old ConfigMap to trigger reload
			if fsEvent.Op&fsnotify.Remove == fsnotify.Remove {
				log.Printf("Remove event on: %s", fsEvent.Name)
				tuned.change.cfg = true
			}

		case err := <-wFs.Errors:
			return fmt.Errorf("Error watching filesystem: %v", err)

		case podEvent, ok := <-wPod.ResultChan():
			if !ok {
				return fmt.Errorf("Pod event watch channel closed.")
			}
			podChangeHandler(podEvent, &tuned)

		case <-tickerPull.C:
			if err := pullLabels(clientset, &tuned, nodeName); err != nil {
				return err
			}

		case <-tickerReload.C:
			if err := timedTunedReloader(&tuned); err != nil {
				return err
			}
		}
	}
}

func retryLoop(f func() error) (err error) {
	var errs int
	const (
		errsMax              = 5
		errsMaxWithinSeconds = 120
	)
	errsTimeStart := time.Now().Unix()
	for {
		err = f()
		if err == nil {
			break
		}
		log.Printf("%s", err.Error())
		if errs++; errs >= errsMax {
			now := time.Now().Unix()
			if (now - errsTimeStart) <= errsMaxWithinSeconds {
				log.Printf("Seen %d errors in %d seconds, terminating...", errs, now-errsTimeStart)
				break
			}
			errs = 0
			errsTimeStart = time.Now().Unix()
		}
		time.Sleep(time.Second * sleepRetry)
	}
	return err
}

func main() {
	rand.Seed(time.Now().UnixNano())
	parseCmdOpts()

	if *boolVersion {
		fmt.Fprintf(os.Stderr, "%s %s\n", PNAME, version)
		os.Exit(0)
	}

	if len(flag.Args()) != 1 {
		flag.Usage()
		os.Exit(1)
	}

	sigs := signalHandler()
	err := retryLoop(changeWatcher)
	signal.Stop(sigs)
	if err != nil {
		panic(err.Error())
	}
}
