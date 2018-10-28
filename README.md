# Containerized tuned daemon for OpenShift

The containerized [tuned](https://github.com/redhat-performance/tuned/)
daemon for [OpenShift](https://openshift.io/).

The daemon is meant to be started by the
[cluster-node-tuning-operator](https://github.com/openshift/cluster-node-tuning-operator).
The operator supplies configuration data to the daemon by volume-mounting ConfigMaps into 
the container.

The tuned pod uses inotify events to catch ConfigMap changes and
reloads the profiles based on a newly recommended profile.  Node label
changes are listened to by using a pull model by querying OpenShift
API server to fetch node labels.
