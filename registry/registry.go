package registry

import (
	"errors"
	"fmt"
	"github.com/skynetservices/skydns/msg"
	"strings"
	"sync"
	"time"
)

var (
	ErrExists    = errors.New("Service already exists in registry")
	ErrNotExists = errors.New("Service does not exist in registry")
)

type Registry interface {
	Add(s msg.Service) error
	Get(domain string) ([]msg.Service, error)
	GetUUID(uuid string) (msg.Service, error)
	GetExpired() []string
	Remove(s msg.Service) error
	RemoveUUID(uuid string) error
	UpdateTTL(uuid string, ttl uint32) error
	Len() int
}

// Creates a new DefaultRegistry
func New() Registry {
	return &DefaultRegistry{
		tree:  newNode(),
		nodes: make(map[string]*node),
	}
}

// Datastore for registered services
type DefaultRegistry struct {
	tree  *node
	nodes map[string]*node
	mutex sync.Mutex
}

// Add service to registry
func (r *DefaultRegistry) Add(s msg.Service) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// TODO: Validate service has correct values, and getRegistryKey returns a valid value
	if _, ok := r.nodes[s.UUID]; ok {
		return ErrExists
	}

	k := getRegistryKey(s)

	n, err := r.tree.add(strings.Split(k, "."), s)

	if err == nil {
		r.nodes[n.value.UUID] = n
	}

	return err
}

// Remove Service specified by UUID
func (r *DefaultRegistry) RemoveUUID(uuid string) error {
	if n, ok := r.nodes[uuid]; ok {
		return r.Remove(n.value)
	}

	return ErrNotExists
}

// Updates the TTL of a service, as well as pushes the expiration time out TTL seconds from now.
// This serves as a ping, for the service to keep SkyDNS aware of it's existence so that it is not expired, and purged.
func (r *DefaultRegistry) UpdateTTL(uuid string, ttl uint32) error {
	if n, ok := r.nodes[uuid]; ok {
		n.value.TTL = ttl
		n.expires = getExpirationTime(ttl)
		return nil
	}

	return ErrNotExists
}

// Remove service from registry
func (r *DefaultRegistry) Remove(s msg.Service) (err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// TODO: Validate service has correct values, and getRegistryKey returns a valid value
	k := getRegistryKey(s)

	err = r.tree.remove(strings.Split(k, "."))

	if err != nil {
		return err
	}

	delete(r.nodes, s.UUID)
	return nil
}

// Retrieve a service based on it's UUID
func (r *DefaultRegistry) GetUUID(uuid string) (s msg.Service, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if s, ok := r.nodes[uuid]; ok {
		return s.value, nil
	}

	return s, ErrNotExists
}

/* Retrieve a list of services from the registry that matches the given domain pattern
 *
 * uuid.host.region.version.service.environment
 * any of these positions may supply the wildcard "any" or "all", to have all values match in this position.
 * additionally, you only need to specify as much of the domain as needed the domain version.service.environment is perfectly acceptable,
 * and will assume "any" for all the ommited subdomain positions
 */
func (r *DefaultRegistry) Get(domain string) ([]msg.Service, error) {
	// TODO: account for version wildcards
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// DNS queries have a trailing .
	if strings.HasSuffix(domain, ".") {
		domain = domain[:len(domain)-1]
	}

	tree := strings.Split(domain, ".")

	// Domains can be partial, and we should assume wildcards for the unsupplied portions
	if len(tree) < 6 {
		pad := 6 - len(tree)
		t := make([]string, pad)

		for i := 0; i < pad; i++ {
			t[i] = "any"
		}

		tree = append(t, tree...)
	}

	return r.tree.get(tree)
}

// Returns a slice of expired UUIDs
func (r *DefaultRegistry) GetExpired() (uuids []string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now()

	for _, n := range r.nodes {
		if now.After(n.expires) {
			uuids = append(uuids, n.value.UUID)
		}
	}

	return
}

func (r *DefaultRegistry) Len() int {
	return r.tree.size()
}

type node struct {
	leaves map[string]*node
	depth  int
	length int

	value   msg.Service
	expires time.Time
}

func newNode() *node {
	return &node{
		leaves: make(map[string]*node),
	}
}

func (n *node) remove(tree []string) error {
	// We are the last element, remove
	if len(tree) == 1 {
		if _, ok := n.leaves[tree[0]]; !ok {
			return ErrNotExists
		} else {
			delete(n.leaves, tree[0])
			n.length--

			return nil
		}
	}

	// Forward removal
	k := tree[len(tree)-1]
	if _, ok := n.leaves[k]; !ok {
		return ErrNotExists
	}

	var err error
	if err = n.leaves[k].remove(tree[:len(tree)-1]); err == nil {
		n.length--

		// Cleanup empty paths
		if n.leaves[k].size() == 0 {
			delete(n.leaves, k)
		}
	}

	return err
}

func (n *node) add(tree []string, s msg.Service) (*node, error) {
	// We are the last element, insert
	if len(tree) == 1 {
		if _, ok := n.leaves[tree[0]]; ok {
			return nil, ErrExists
		}

		n.leaves[tree[0]] = &node{
			value:   s,
			expires: getExpirationTime(s.TTL),
			leaves:  make(map[string]*node),
			depth:   n.depth + 1,
		}

		n.length++

		return n.leaves[tree[0]], nil
	}

	// Forward entry
	k := tree[len(tree)-1]
	if _, ok := n.leaves[k]; !ok {
		n.leaves[k] = newNode()
		n.leaves[k].depth = n.depth + 1
	}

	newNode, err := n.leaves[k].add(tree[:len(tree)-1], s)
	if err != nil {
		return nil, err
	}

	// This node length should account for all nodes below it
	n.length++
	return newNode, nil
}

func (n *node) size() int {
	return n.length
}

func (n *node) get(tree []string) (services []msg.Service, err error) {
	// We've hit the bottom
	if len(tree) == 1 {
		switch tree[0] {
		case "all", "any":
			if len(n.leaves) == 0 {
				return services, ErrNotExists
			}

			for _, s := range n.leaves {
				services = append(services, s.value)
			}
		default:
			if _, ok := n.leaves[tree[0]]; !ok {
				return services, ErrNotExists
			}

			services = append(services, n.leaves[tree[0]].value)
		}

		return
	}

	k := tree[len(tree)-1]

	switch k {
	case "all", "any":
		if len(n.leaves) == 0 {
			return services, ErrNotExists
		}

		var success bool
		for _, l := range n.leaves {
			if s, e := l.get(tree[:len(tree)-1]); e == nil {
				services = append(services, s...)
				success = true
			}
		}

		if !success {
			return services, ErrNotExists
		}
	default:
		if _, ok := n.leaves[k]; !ok {
			return services, ErrNotExists
		}

		return n.leaves[k].get(tree[:len(tree)-1])
	}

	return
}

func getRegistryKey(s msg.Service) string {
	return strings.ToLower(fmt.Sprintf("%s.%s.%s.%s.%s.%s", s.UUID, s.Host, s.Region, strings.Replace(s.Version, ".", "-", -1), s.Name, s.Environment))
}

func getExpirationTime(ttl uint32) time.Time {
	return time.Now().Add(time.Duration(ttl) * time.Second)
}
