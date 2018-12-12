# Containerized tuned daemon for OpenShift

The containerized [tuned](https://github.com/redhat-performance/tuned/)
daemon for [OpenShift](https://openshift.io/).

The daemon is meant to be started by the
[cluster-node-tuning-operator](https://github.com/openshift/cluster-node-tuning-operator).
The operator supplies configuration data to the daemon by volume-mounting ConfigMaps into
the container.

The tuned pod:
  - watches for inotify events to catch changes to the profile and tuned sections of the `recommend.conf` ConfigMap.
  - watches for node and pod label changes of all pods running on the node that the tuned pod runs on.

A "recommended" tuned profile is (re-)applied based on the changes above.
