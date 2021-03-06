package gremlin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/eonpatapon/gremlin"
	"github.com/google/go-cmp/cmp"
	logging "github.com/op/go-logging"
)

var (
	log = logging.MustGetLogger("gremlin")
	// ErrIncompleteVertex indicates that the vertex is missing properties
	// and will not be put in gremlin-server
	ErrIncompleteVertex = errors.New("vertex is incomplete")
)

// ServerBackend handles operations against gremlin-server
type ServerBackend struct {
	client               *gremlin.Client
	connected            atomic.Value
	connectedHandlers    []func()
	disconnectedHandlers []func(error)
}

// NewServerBackend is the connection to the gremlin-server
func NewServerBackend(gremlinURI string) *ServerBackend {
	b := &ServerBackend{
		client:               gremlin.NewClient(gremlinURI),
		connectedHandlers:    []func(){},
		disconnectedHandlers: []func(error){},
	}
	b.connected.Store(false)
	b.client.AddConnectedHandler(b.onConnected)
	b.client.AddDisconnectedHandler(b.onDisconnected)
	return b
}

// AddConnectedHandler runs handler when client is connected
func (b *ServerBackend) AddConnectedHandler(h func()) {
	b.connectedHandlers = append(b.connectedHandlers, h)
}

// AddDisconnectedHandler runs handler when connection is closed
func (b *ServerBackend) AddDisconnectedHandler(h func(error)) {
	b.disconnectedHandlers = append(b.disconnectedHandlers, h)
}

func (b *ServerBackend) IsConnected() bool {
	return b.connected.Load().(bool)
}

func (b *ServerBackend) onConnected() {
	b.connected.Store(true)
	for _, h := range b.connectedHandlers {
		h()
	}
}

func (b *ServerBackend) onDisconnected(err error) {
	b.connected.Store(false)
	for _, h := range b.disconnectedHandlers {
		h(err)
	}
}

// Start starts the underlying client
func (b *ServerBackend) Start() {
	b.client.Connect()
}

// StartAsync starts the underlying client
func (b *ServerBackend) StartAsync() {
	b.client.ConnectAsync()
}

// Stop stops the underlying client
func (b *ServerBackend) Stop() {
	b.client.Disconnect()
}

// Connected returns true if the client is connected
func (b *ServerBackend) Connected() bool {
	return b.connected.Load().(bool)
}

// Send request to underlying client
func (b *ServerBackend) Send(req *gremlin.Request) ([]byte, error) {
	return b.client.Send(req)
}

// CreateVertex creates a vertex and its associated edges
func (b *ServerBackend) CreateVertex(v Vertex) error {
	// UpdateVertex handle creation as well
	return b.UpdateVertex(v)
}

// CreateEdge create an edge between it's vertices
func (b *ServerBackend) CreateEdge(e Edge) error {
	props, bindings := edgePropertiesQuery(e.Properties)
	bindings["_outv"] = e.OutV
	bindings["_outv_label"] = e.OutVLabel
	bindings["_inv"] = e.InV
	bindings["_inv_label"] = e.InVLabel
	bindings["_label"] = e.Label

	// make sure that the other side of the edge exists
	// if it doesn't we create it with the _missing property
	// eventually it will be updated later
	var query string
	// for ref/parent
	if e.OutVLabel == "" {
		query = `g.V(_outv).as('outv').coalesce(
			g.V(_inv),
			g.addV(_inv_label)
			 .property(id, _inv)
			 .property('fq_name', ['_missing'])
			 .property('_missing', true)
			 .property('deleted', 0)
		).addE(_label).from('outv')` + props + `.iterate()`
	}
	// for children/backref
	if e.InVLabel == "" {
		query = `g.V(_inv).as('inv').coalesce(
			g.V(_outv),
			g.addV(_outv_label)
			 .property(id, _outv)
			 .property('fq_name', ['_missing'])
			 .property('_missing', true)
			 .property('deleted', 0)
		).addE(_label).to('inv')` + props + `.iterate()`
	}

	_, err := b.Send(
		gremlin.Query(query).Bindings(bindings),
	)
	if err == gremlin.ErrStatusInvalidRequestArguments {
		log.Errorf("Query: %s, Bindings: %s", query, bindings)
	}
	return err
}

// UpdateVertex updates properties and edges of the given vertex
func (b *ServerBackend) UpdateVertex(v Vertex) error {
	if v.Label == "" {
		return ErrIncompleteVertex
	}
	props, bindings := vertexPropertiesQuery(v.Properties)
	bindings["_id"] = v.ID
	bindings["_label"] = v.Label
	query := `g.V().hasId(_id).fold().
			  coalesce(unfold().sideEffect(properties().drop()),
					   addV(_label).property(id, _id))
			 ` + props + `.iterate()`
	_, err := b.Send(
		gremlin.Query(query).Bindings(bindings),
	)
	if err != nil {
		if err == gremlin.ErrStatusInvalidRequestArguments {
			log.Errorf("Query: %s, Bindings: %s", query, bindings)
		}
		return err
	}
	return b.updateVertexEdges(v)
}

// UpdateEdge updates properties of the given edge
func (b *ServerBackend) UpdateEdge(e Edge) error {
	props, bindings := edgePropertiesQuery(e.Properties)
	bindings["_inv"] = e.InV
	bindings["_outv"] = e.OutV
	query := `g.V(_inv).bothE().where(otherV().hasId(_outv))
			   .sideEffect(properties().drop())` + props + `.iterate()`
	_, err := b.Send(
		gremlin.Query(query).Bindings(bindings),
	)
	if err == gremlin.ErrStatusInvalidRequestArguments {
		log.Errorf("Query: %s, Bindings: %s", query, bindings)
	}
	return err
}

// DeleteVertex deletes the given vertex
func (b *ServerBackend) DeleteVertex(v Vertex) error {
	_, err := b.Send(
		gremlin.Query(`g.V(_id).drop()`).Bindings(
			gremlin.Bind{
				"_id": v.ID,
			},
		),
	)
	if err != nil {
		return err
	}
	return nil
}

// DeleteEdge deletes the given edge
func (b *ServerBackend) DeleteEdge(e Edge) error {
	_, err := b.Send(
		gremlin.Query("g.V(_inv).bothE().where(otherV().hasId(_outv)).drop()").Bindings(
			gremlin.Bind{
				"_inv":  e.InV,
				"_outv": e.OutV,
			},
		),
	)
	return err
}

// UpdateVertexProperty set the given property on the vertex
func (b *ServerBackend) UpdateVertexProperty(v Vertex, name string, value interface{}) error {
	if v.Label == "" {
		return ErrIncompleteVertex
	}
	query := `g.V(_id).property(_name, _value).iterate()`
	_, err := b.Send(
		gremlin.Query(query).Bindings(gremlin.Bind{
			"_id":    v.ID,
			"_name":  name,
			"_value": value,
		}),
	)
	if err != nil {
		return err
	}
	return nil
}

func (b *ServerBackend) currentVertexEdges(v Vertex) (edges []Edge, err error) {
	var data []byte
	data, err = b.Send(
		gremlin.Query(`g.V(_id).bothE()`).Bindings(
			gremlin.Bind{
				"_id": v.ID.String(),
			},
		),
	)
	if err != nil {
		return nil, err
	}
	json.Unmarshal(data, &edges)

	return edges, err
}

func (b *ServerBackend) diffVertexEdges(v Vertex) ([]Edge, []Edge, []Edge, error) {
	var (
		toAdd    []Edge
		toRemove []Edge
		toUpdate []Edge
	)

	currentEdges, err := b.currentVertexEdges(v)
	if err != nil {
		return toAdd, toUpdate, toRemove, err
	}

	var vertexEdges []Edge
	for _, edges := range v.OutE {
		vertexEdges = append(vertexEdges, edges...)
	}
	for _, edges := range v.InE {
		vertexEdges = append(vertexEdges, edges...)
	}

	for _, l1 := range vertexEdges {
		found := false
		update := false
		for _, l2 := range currentEdges {
			if l1.InV == l2.InV && l1.OutV == l2.OutV && l1.Label == l2.Label {
				found = true
				if !cmp.Equal(l1.Properties, l2.Properties) {
					update = true
				}
				break
			}
		}
		if !found {
			toAdd = append(toAdd, l1)
		}
		if found && update {
			toUpdate = append(toUpdate, l1)
		}
	}

	for _, l1 := range currentEdges {
		found := false
		for _, l2 := range vertexEdges {
			if l1.InV == l2.InV && l1.OutV == l2.OutV && l1.Label == l2.Label {
				found = true
				break
			}
		}
		if !found {
			toRemove = append(toRemove, l1)
		}
	}

	return toAdd, toUpdate, toRemove, nil
}

func (b *ServerBackend) updateVertexEdges(v Vertex) error {
	toAdd, toUpdate, toRemove, err := b.diffVertexEdges(v)
	if err != nil {
		return err
	}

	for _, edge := range toAdd {
		err = b.CreateEdge(edge)
		if err != nil {
			return err
		}
	}

	for _, edge := range toUpdate {
		err = b.UpdateEdge(edge)
		if err != nil {
			return err
		}
	}

	for _, edge := range toRemove {
		err = b.DeleteEdge(edge)
		if err != nil {
			return err
		}
	}

	return nil
}

func vertexPropertiesQuery(propList map[string][]Property) (string, gremlin.Bind) {
	var buffer bytes.Buffer
	bindings := gremlin.Bind{}
	propNames := make([]string, len(propList))
	i := 0
	for name := range propList {
		propNames[i] = name
		i++
	}
	sort.SliceStable(propNames, func(i, j int) bool {
		return propNames[i] < propNames[j]
	})
	for _, propName := range propNames {
		for i, value := range propList[propName] {
			bindName := fmt.Sprintf(`_%s_%d`, strings.Replace(propName, `.`, `_`, -1), i)
			buffer.WriteString(`.property(`)
			if len(propList[propName]) > 1 {
				buffer.WriteString(`list,`)
			}
			buffer.WriteString(fmt.Sprintf(`'%s',`, propName))
			buffer.WriteString(bindName)
			buffer.WriteString(`)`)
			bindings[bindName] = value.Value
		}
	}
	return buffer.String(), bindings
}

func edgePropertiesQuery(propList map[string]Property) (string, gremlin.Bind) {
	var buffer bytes.Buffer
	bindings := gremlin.Bind{}
	propNames := make([]string, 0)
	for name, prop := range propList {
		// gremlin does not allow null values in edge properties
		if prop.Value != nil {
			propNames = append(propNames, name)
		}
	}
	sort.SliceStable(propNames, func(i, j int) bool {
		return propNames[i] < propNames[j]
	})
	for _, propName := range propNames {
		bindName := fmt.Sprintf(`_%s`, strings.Replace(propName, `.`, `_`, -1))
		buffer.WriteString(`.property(`)
		buffer.WriteString(fmt.Sprintf(`'%s',`, propName))
		buffer.WriteString(bindName)
		buffer.WriteString(`)`)
		bindings[bindName] = propList[propName].Value
	}
	return buffer.String(), bindings
}
