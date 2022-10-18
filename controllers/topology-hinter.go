/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// EndpointSliceReconciler reconciles a Memcached object
type EndpointSliceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	myFinalizerName     = "topology-hinter.maxsum.io/finalizer"
	hintAnnotation      = "maxsum.io/topology-hint"
	zoneLabel           = "topology.kubernetes.io/zone"
	regionLabel         = "topology.kubernetes.io/region"
	cityLabel           = "geo.maxsum.io/city"
	countryLabel        = "geo.maxsum.io/country"
	subContinentLabel   = "geo.maxsum.io/subcontinent"
	continentLabel      = "geo.maxsum.io/continent"
	servicenameLabel    = "kubernetes.io/service-name"
	ipFamilyLabel       = "topology-hinter.maxsum.io/ip-family"
	sliceManagedByLabel = "endpointslice.kubernetes.io/managed-by"
	sliceManager        = "topology-hinter.maxsum.io"
	sliceController     = "endpointslice-controller.k8s.io"

	numSliceAnnotation = "topology-hinter.maxsum.io/num-slices"
	ipv4Annotation     = "topology-hinter.maxsum.io/ipv4"
	ipv6Annotation     = "topology-hinter.maxsum.io/ipv6"
)

type k8sObj interface {
	GetName() string
	GetNamespace() string
}

func getNamespacedName(o k8sObj) types.NamespacedName {
	return types.NamespacedName{
		Namespace: o.GetNamespace(),
		Name:      o.GetName(),
	}
}

type geoTree struct {
	label       string                  // Label of this level
	endpointsv4 []*discoveryv1.Endpoint // Available Endpoints IPv4
	endpointsv6 []*discoveryv1.Endpoint // Available Endpoints IPv6
	children    map[string]*geoTree     // nextLevel, key = label (this level)'s content
	parent      *geoTree
}

type zone struct {
	*geoTree
	name string // name of zone
}

func buildGeoBranch(nodes []*corev1.Node, labelList []string, labelContent string) (*geoTree, []*zone) {
	if len(labelList) == 0 {
		tree := &geoTree{
			label:       "",
			endpointsv4: make([]*discoveryv1.Endpoint, 0),
			endpointsv6: make([]*discoveryv1.Endpoint, 0),
			children:    nil,
			parent:      nil,
		}
		return tree, []*zone{{geoTree: tree, name: labelContent}}
	}
	label := labelList[0]
	tree := &geoTree{
		label:       label,
		endpointsv4: make([]*discoveryv1.Endpoint, 0),
		endpointsv6: make([]*discoveryv1.Endpoint, 0),
		children:    make(map[string]*geoTree),
		parent:      nil,
	}
	zones := make([]*zone, 0)
	group := make(map[string][]*corev1.Node)
	for _, node := range nodes {
		v := node.Labels[label]
		group[v] = append(group[v], node)
	}
	for v, nodes := range group {
		child, z := buildGeoBranch(nodes, labelList[1:], v)
		child.parent = tree
		tree.children[v] = child
		zones = append(zones, z...)
	}
	return tree, zones
}

func buildGeoTree(nodeList *corev1.NodeList) (*geoTree, []*zone, error) {
	labelList := []string{continentLabel, subContinentLabel, countryLabel, cityLabel, regionLabel, zoneLabel}
	availableLabelList := make([]string, 0, len(labelList))
	for _, label := range labelList {
		canUseLabel := true
		// Only use labels set on all nodes
		for _, node := range nodeList.Items {
			_, ok := node.Labels[label]
			if !ok {
				// skip non-existing label
				canUseLabel = false
				break
			}
		}
		if canUseLabel {
			availableLabelList = append(availableLabelList, label)
		}
	}
	if len(availableLabelList) == 0 {
		return nil, nil, errors.New("no label can be used")
	}
	nodes := make([]*corev1.Node, len(nodeList.Items))
	for i := range nodeList.Items {
		nodes[i] = &nodeList.Items[i]
	}
	tree, zones := buildGeoBranch(nodes, availableLabelList, "")
	return tree, zones, nil
}

func (r *EndpointSliceReconciler) deleteSlice(ctx context.Context, name string, namespace string) error {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				sliceManagedByLabel: sliceManager,
			},
		},
	}
	if err := r.Delete(ctx, slice); err != nil {
		log.Log.Error(err, "failed to delete endpointSlice")
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	log.Log.Info("endpointslice " + getNamespacedName(slice).String() + " deleted")
	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Memcached object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *EndpointSliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	shouldHint := true
	endpointUnderControl := false
	useIPv4 := false
	useIPv6 := false
	boolTrue := true
	boolFalse := false
	var err error

	// When Service/Endpoints Updated
	endpoint := &corev1.Endpoints{}
	service := &corev1.Service{}

	log.Log.Info("Triggered by service/endpoints: " + req.NamespacedName.String())
	// Get Endpoint
	if err := r.Get(ctx, req.NamespacedName, endpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Log.Error(err, "unable to fetch Endpoint: "+req.NamespacedName.String())
		return ctrl.Result{}, err
	}
	// Get the old number of slices
	oldNumSlice := 0
	if v, ok := endpoint.Annotations[numSliceAnnotation]; ok {
		endpointUnderControl = true
		oldNumSlice, err = strconv.Atoi(v)
		log.Log.Info("found " + v + " old endpointslices")
		if err != nil {
			log.Log.Error(err, "failed to read number of slices")
			return ctrl.Result{}, err
		}
	}

	// Delete on endpoint deletion
	if !endpoint.ObjectMeta.DeletionTimestamp.IsZero() {
		if err = r.DeleteAllOf(ctx, &discoveryv1.EndpointSlice{}, client.MatchingLabels{
			sliceManagedByLabel: sliceManager,
			servicenameLabel:    endpoint.Name,
		}, client.InNamespace(endpoint.Namespace)); err != nil {
			log.Log.Error(err, "failed to delete endpointSlice")
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(endpoint, myFinalizerName)
		if err := r.Update(ctx, endpoint); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			log.Log.Error(err, "failed to delete finalizer from endpoints"+req.NamespacedName.String())
			return ctrl.Result{}, err
		}
		log.Log.Info("endpoints " + req.NamespacedName.String() + " deleting, removed all slices")
		return ctrl.Result{}, nil
	}

	// Get Service
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Log.Error(err, "unable to fetch Service: "+endpoint.GetName())
		return ctrl.Result{}, err
	}

	{
		// Check service uses selectors
		useSelectors := len(service.Spec.Selector) > 0
		// Check if hint annotation presents
		hint, ok := service.Annotations[hintAnnotation]
		hasAnnotation := ok && strings.ToLower(hint) == "true"
		// Check ip families
		for _, ipFamily := range service.Spec.IPFamilies {
			switch ipFamily {
			case corev1.IPv4Protocol:
				useIPv4 = true
			case corev1.IPv6Protocol:
				useIPv6 = true
			default:
				continue
			}
		}

		if !hasAnnotation || !useSelectors {
			if endpointUnderControl {
				// Cannot return control to endpointslice controller
				// continue to sync with endpoints without hints
				log.Log.Info("service " + req.NamespacedName.String() + " no longer has annotation, no hinting applied")
				shouldHint = false
			} else {
				// Ignored
				log.Log.Info("service " + req.NamespacedName.String() + " has no annotation, ignored")
				return ctrl.Result{}, nil
			}
		}
	}

	// Get All Nodes
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		log.Log.Error(err, "unable to get Nodes")
		return ctrl.Result{}, err
	}
	log.Log.Info("Get nodelist")
	// require zone to be effective
	for _, node := range nodeList.Items {
		_, ok := node.Labels[zoneLabel]
		if !ok {
			shouldHint = false
			log.Log.Info("node " + node.Name + " lack zone label, no hinting applied")
			break
		}
	}
	var tree *geoTree
	var zones []*zone
	if shouldHint {
		// build geo tree
		tree, zones, err = buildGeoTree(nodeList)
		if tree == nil || err != nil {
			log.Log.Error(err, "unable to build Geo Tree")
			return ctrl.Result{}, err
		}
		log.Log.Info("geo tree built")
	}

	// Copy Endpoints -> EndpointSlice
	for i, subset := range endpoint.Subsets {
		var slicev4 *discoveryv1.EndpointSlice
		var slicev6 *discoveryv1.EndpointSlice
		if useIPv4 {
			slicev4 = &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: endpoint.GetNamespace(),
					Name:      endpoint.GetName() + "-ipv4-" + strconv.Itoa(i),
					Labels: map[string]string{
						sliceManagedByLabel: sliceManager,
						servicenameLabel:    service.GetName(),
						ipFamilyLabel:       "IPv4",
					},
				},
				AddressType: discoveryv1.AddressTypeIPv4,
				Ports:       make([]discoveryv1.EndpointPort, 0, len(subset.Ports)),
				Endpoints:   make([]discoveryv1.Endpoint, 0, len(subset.Addresses)+len(subset.NotReadyAddresses)),
			}
		}
		if useIPv6 {
			slicev6 = &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: endpoint.GetNamespace(),
					Name:      endpoint.GetName() + "-ipv6-" + strconv.Itoa(i),
					Labels: map[string]string{
						sliceManagedByLabel: sliceManager,
						servicenameLabel:    service.GetName(),
						ipFamilyLabel:       "IPv6",
					},
				},
				AddressType: discoveryv1.AddressTypeIPv6,
				Ports:       make([]discoveryv1.EndpointPort, 0, len(subset.Ports)),
				Endpoints:   make([]discoveryv1.Endpoint, 0, len(subset.Addresses)+len(subset.NotReadyAddresses)),
			}
		}
		log.Log.Info("slice " + getNamespacedName(endpoint).String() + "-" + strconv.Itoa(i) + " temporarily created")
		for _, port := range subset.Ports {
			esPort := discoveryv1.EndpointPort{
				Name:        &port.Name,
				Port:        &port.Port,
				Protocol:    &port.Protocol,
				AppProtocol: port.AppProtocol,
			}
			if slicev4 != nil {
				slicev4.Ports = append(slicev4.Ports, *esPort.DeepCopy())
			}
			if slicev6 != nil {
				slicev6.Ports = append(slicev6.Ports, *esPort.DeepCopy())
			}
		}
		for _, addr := range subset.Addresses {
			// Ignore without target pod
			if addr.TargetRef == nil || addr.TargetRef.Kind != "Pod" {
				continue
			}
			pointv4 := discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{
					Ready: &boolTrue,
				},
				NodeName:  addr.NodeName,
				TargetRef: addr.TargetRef,
			}
			pointv6 := discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{
					Ready: &boolTrue,
				},
				NodeName:  addr.NodeName,
				TargetRef: addr.TargetRef,
			}
			log.Log.Info("endpoint to " + addr.IP + " created")
			// Get Pod
			pod := &corev1.Pod{}
			if err = r.Get(ctx, types.NamespacedName{
				Namespace: addr.TargetRef.Namespace,
				Name:      addr.TargetRef.Name}, pod); err != nil {
				log.Log.Error(err, "unable to get pod "+addr.TargetRef.Namespace+"/"+addr.TargetRef.Name)
				return ctrl.Result{}, err
			}
			if pod.DeletionTimestamp.IsZero() {
				pointv4.Conditions.Terminating = &boolFalse
				pointv6.Conditions.Terminating = &boolFalse
			} else {
				pointv4.Conditions.Terminating = &boolTrue
				pointv6.Conditions.Terminating = &boolTrue
			}
			for _, podIP := range pod.Status.PodIPs {
				ip := net.ParseIP(podIP.IP)
				if ip == nil {
					continue
				}
				if ip.To4() != nil {
					pointv4.Addresses = append(pointv4.Addresses, podIP.IP)
				} else if ip.To16() != nil {
					pointv6.Addresses = append(pointv6.Addresses, podIP.IP)
				}
			}
			// Find Node
			node := corev1.Node{}
			zone := ""
			for _, n := range nodeList.Items {
				if n.Name == *addr.NodeName {
					node = n
					break
				}
			}
			if node.Name != *addr.NodeName {
				// Node list has changed, wait for next update
				log.Log.Info("node " + *addr.NodeName + " is not in nodelist, wait for next update")
				return ctrl.Result{}, nil
			}
			// Fill Zone
			if z, ok := node.Labels[zoneLabel]; ok {
				zone = z
				pointv4.Zone = &zone
				pointv6.Zone = &zone
			}
			if slicev4 != nil {
				slicev4.Endpoints = append(slicev4.Endpoints, *(pointv4.DeepCopy()))
			}
			if slicev6 != nil {
				slicev6.Endpoints = append(slicev6.Endpoints, *(pointv6.DeepCopy()))
			}
			log.Log.Info("endpoint " + addr.IP + " appended to slice")
			// Add to geo tree
			if shouldHint {
				next := tree
				for {
					if slicev4 != nil {
						next.endpointsv4 = append(next.endpointsv4, &slicev4.Endpoints[len(slicev4.Endpoints)-1])
					}
					if slicev6 != nil {
						next.endpointsv6 = append(next.endpointsv6, &slicev6.Endpoints[len(slicev6.Endpoints)-1])
					}
					if next.label == "" {
						break
					}
					v := node.Labels[next.label]
					if next.children[v] == nil {
						log.Log.Error(errors.New("going down geo tree failed, no child is found for label "+next.label+":"+v), "")
						return ctrl.Result{}, err
					}
					next = next.children[v]
				}
				log.Log.Info("endpoint " + addr.IP + " appended to geo tree")
			}
		}
		log.Log.Info("slice " + getNamespacedName(endpoint).String() + "-" + strconv.Itoa(i) + " addresses filled")
		// Sort out hints
		if shouldHint && useIPv4 {
			for _, z := range zones {
				// Ensure all zones get at least one endpoint
				branch := z.geoTree
				for branch != nil {
					if len(branch.endpointsv4) > 0 {
						log.Log.Info("hinting: found IPv4 endpoints in level "+branch.label+" for zone "+z.name, "num", len(branch.endpointsv4))
						// Found endpoints at this level, set forzones
						for _, p := range branch.endpointsv4 {
							if p.Hints == nil {
								p.Hints = &discoveryv1.EndpointHints{}
							}
							p.Hints.ForZones = append(p.Hints.ForZones, discoveryv1.ForZone{
								Name: z.name,
							})
							if len(p.Hints.ForZones) > 8 {
								log.Log.Error(errors.New("hinting: forzones is >8, cannot fulfill. Ignored"), "")
								shouldHint = false
								break
							}
							log.Log.Info("hinting: use endpoint " + *p.NodeName + " for zone " + z.name)
						}
						break
					}
					// Go up one level
					branch = branch.parent
				}
				if !shouldHint {
					for _, point := range slicev4.Endpoints {
						point.Hints = nil
					}
					break
				} else {
					log.Log.Info("slice " + getNamespacedName(slicev4).String() + " hints added")
				}
			}
		}
		if shouldHint && useIPv6 {
			for _, z := range zones {
				// Ensure all zones get at least one endpoint
				branch := z.geoTree
				for branch != nil {
					if len(branch.endpointsv6) > 0 {
						log.Log.Info("hinting: found IPv6 endpoints in level "+branch.label+" for zone "+z.name, "num", len(branch.endpointsv6))
						// Found endpoints at this level, set forzones
						for _, p := range branch.endpointsv6 {
							if p.Hints == nil {
								p.Hints = &discoveryv1.EndpointHints{}
							}
							p.Hints.ForZones = append(p.Hints.ForZones, discoveryv1.ForZone{
								Name: z.name,
							})
							if len(p.Hints.ForZones) > 8 {
								log.Log.Error(errors.New("hinting: forzones is >8, cannot fulfill. Ignored"), "")
								shouldHint = false
								break
							}
							log.Log.Info("hinting: use endpoint " + *p.NodeName + " for zone " + z.name)
						}
						break
					}
					// Go up one level
					branch = branch.parent
				}
				if !shouldHint {
					for _, point := range slicev6.Endpoints {
						point.Hints = nil
					}
					break
				} else {
					log.Log.Info("slice " + getNamespacedName(slicev6).String() + " hints added")
				}
			}
		}
		// Add NotReadyAddresses
		for _, addr := range subset.NotReadyAddresses {
			// Ignore without target pod
			if addr.TargetRef == nil || addr.TargetRef.Kind != "Pod" {
				continue
			}
			pointv4 := discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{
					Ready: &boolTrue,
				},
				NodeName:  addr.NodeName,
				TargetRef: addr.TargetRef,
			}
			pointv6 := discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{
					Ready: &boolTrue,
				},
				NodeName:  addr.NodeName,
				TargetRef: addr.TargetRef,
			}
			log.Log.Info("endpoint to " + addr.IP + " created")
			// Get Pod
			pod := &corev1.Pod{}
			if err = r.Get(ctx, types.NamespacedName{
				Namespace: addr.TargetRef.Namespace,
				Name:      addr.TargetRef.Name}, pod); err != nil {
				log.Log.Error(err, "unable to get pod "+addr.TargetRef.Namespace+"/"+addr.TargetRef.Name)
				return ctrl.Result{}, err
			}
			if pod.DeletionTimestamp.IsZero() {
				pointv4.Conditions.Terminating = &boolFalse
				pointv6.Conditions.Terminating = &boolFalse
			} else {
				pointv4.Conditions.Terminating = &boolTrue
				pointv6.Conditions.Terminating = &boolTrue
			}
			for _, podIP := range pod.Status.PodIPs {
				ip := net.ParseIP(podIP.IP)
				if ip == nil {
					continue
				}
				if ip.To4() != nil {
					pointv4.Addresses = append(pointv4.Addresses, podIP.IP)
				} else if ip.To16() != nil {
					pointv6.Addresses = append(pointv6.Addresses, podIP.IP)
				}
			}
			if slicev4 != nil {
				slicev4.Endpoints = append(slicev4.Endpoints, *(pointv4.DeepCopy()))
			}
			if slicev6 != nil {
				slicev6.Endpoints = append(slicev6.Endpoints, *(pointv6.DeepCopy()))
			}
		}
		log.Log.Info("slice " + getNamespacedName(endpoint).String() + "-" + strconv.Itoa(i) + " not ready addresses filled")

		if slicev4 != nil {
			if _, hasIPv4 := endpoint.Annotations[ipv4Annotation]; hasIPv4 && i < oldNumSlice {
				err = r.Update(ctx, slicev4)
			} else {
				err = r.Create(ctx, slicev4)
				if apierrors.IsAlreadyExists(err) {
					r.Delete(ctx, slicev4)
					err = r.Create(ctx, slicev4)
				}
			}
			if err != nil {
				log.Log.Error(err, "failed to update/create endpointSlice")
				return ctrl.Result{}, err
			}
		}
		if slicev6 != nil {
			if _, hasIPv6 := endpoint.Annotations[ipv6Annotation]; hasIPv6 && i < oldNumSlice {
				err = r.Update(ctx, slicev6)
			} else {
				err = r.Create(ctx, slicev6)
				if apierrors.IsAlreadyExists(err) {
					r.Delete(ctx, slicev6)
					err = r.Create(ctx, slicev6)
				}
			}
			if err != nil {
				log.Log.Error(err, "failed to update/create endpointSlice")
				return ctrl.Result{}, err
			}
		}
		log.Log.Info("endpointslice " + getNamespacedName(endpoint).String() + "-" + strconv.Itoa(i) + " created/updated")
	}

	// Delete slices more than desired state
	for i := len(endpoint.Subsets); i < oldNumSlice; i++ {
		if useIPv4 {
			err = r.deleteSlice(ctx, endpoint.Name+"-ipv4-"+strconv.Itoa(i), endpoint.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		if useIPv6 {
			err = r.deleteSlice(ctx, endpoint.Name+"-ipv6-"+strconv.Itoa(i), endpoint.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if _, hasIPv4 := endpoint.Annotations[ipv4Annotation]; !useIPv4 && hasIPv4 {
		if err = r.DeleteAllOf(ctx, &discoveryv1.EndpointSlice{}, client.MatchingLabels{
			sliceManagedByLabel: sliceManager,
			servicenameLabel:    endpoint.Name,
			ipFamilyLabel:       "IPv4",
		}, client.InNamespace(endpoint.Namespace)); err != nil {
			log.Log.Error(err, "failed to delete endpointSlice")
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if _, hasIPv6 := endpoint.Annotations[ipv6Annotation]; !useIPv6 && hasIPv6 {
		if err = r.DeleteAllOf(ctx, &discoveryv1.EndpointSlice{}, client.MatchingLabels{
			sliceManagedByLabel: sliceManager,
			servicenameLabel:    endpoint.Name,
			ipFamilyLabel:       "IPv6",
		}, client.InNamespace(endpoint.Namespace)); err != nil {
			log.Log.Error(err, "failed to delete endpointSlice")
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// Update Endpoints
	{
		updateEndpoints := false
		if !controllerutil.ContainsFinalizer(endpoint, myFinalizerName) {
			controllerutil.AddFinalizer(endpoint, myFinalizerName)
			updateEndpoints = true
		}
		if oldNumSlice != len(endpoint.Subsets) {
			if endpoint.Annotations == nil {
				endpoint.Annotations = make(map[string]string)
			}
			endpoint.Annotations[numSliceAnnotation] = strconv.Itoa(len(endpoint.Subsets))
			updateEndpoints = true
		}
		if _, hasIPv4 := endpoint.Annotations[ipv4Annotation]; useIPv4 != hasIPv4 {
			updateEndpoints = true
			if useIPv4 {
				endpoint.Annotations[ipv4Annotation] = ""
			} else {
				delete(endpoint.Annotations, ipv4Annotation)
			}
		}
		if _, hasIPv6 := endpoint.Annotations[ipv6Annotation]; useIPv6 != hasIPv6 {
			updateEndpoints = true
			if useIPv6 {
				endpoint.Annotations[ipv6Annotation] = ""
			} else {
				delete(endpoint.Annotations, ipv6Annotation)
			}
		}
		if updateEndpoints {
			err = r.Update(ctx, endpoint)
			if err != nil {
				log.Log.Error(err, "failed to update endpoint")
				return ctrl.Result{}, err
			}
			log.Log.Info("endpoint " + getNamespacedName(endpoint).String() + " updated")
		}
	}

	// Delete endpointslice created by kubernetes
	slices := &discoveryv1.EndpointSliceList{}
	if err = r.List(ctx, slices, client.MatchingLabels{
		sliceManagedByLabel: sliceController,
		servicenameLabel:    req.Name,
	}, client.InNamespace(req.Namespace)); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Log.Error(err, "failed to find endpointslice from controller")
			return ctrl.Result{}, err
		}
	}
	for _, slice := range slices.Items {
		slice.Labels[sliceManagedByLabel] = ""
		if err = r.Update(ctx, &slice); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			log.Log.Error(err, "failed to update EndpointSlice from controller")
			return ctrl.Result{}, err
		}
		if err = r.Delete(ctx, &slice); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			log.Log.Error(err, "failed to delete EndpointSlice from controller")
			return ctrl.Result{}, err
		}
	}

	log.Log.Info("mirror endpointslice from endpoints " + getNamespacedName(endpoint).String() + " completed")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EndpointSliceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Endpoints{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				ignoredAnnos := []string{numSliceAnnotation, ipv4Annotation, ipv6Annotation}
				// Filter the changes the hinter makes
				oldFinalizers := e.ObjectOld.GetFinalizers()
				newFinalizers := e.ObjectNew.GetFinalizers()
				finalizerDiff := map[string]int{}
				for _, k := range oldFinalizers {
					finalizerDiff[k]++
				}
				for _, k := range newFinalizers {
					finalizerDiff[k]--
				}
				for k, v := range finalizerDiff {
					if v != 0 && k == myFinalizerName {
						return false
					}
				}
				oldAnnos := e.ObjectOld.GetAnnotations()
				newAnnos := e.ObjectNew.GetAnnotations()
				annoDiff := map[string]int{}
				for k := range oldAnnos {
					annoDiff[k]++
				}
				for k := range newAnnos {
					annoDiff[k]--
				}
				for k, v := range annoDiff {
					if v == 0 && oldAnnos[k] == newAnnos[k] {
						continue
					}
					for _, ik := range ignoredAnnos {
						if k == ik {
							return false
						}
					}
				}
				return true
			},
		}).
		Complete(r)
	if err != nil {
		return err
	}
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(_ event.CreateEvent) bool {
				return false
			},
			DeleteFunc: func(_ event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(_ event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
	return err
}
