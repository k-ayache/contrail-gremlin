package main

import (
	"github.com/eonpatapon/gremlin"
)

var networkDefaultFields = []string{
	"id",
	"tenant_id",
	"name",
	"description",
	"router:external",
	"shared",
	"subnets",
	"status",
	"admin_state_up",
	"port_security_enabled",
	"created_at",
	"updated_at",
}

func listNetworks(r Request, app *App) ([]byte, error) {
	var (
		query    = &gremlinQuery{}
		bindings = gremlin.Bind{}
	)

	query.Add(`g.V().hasLabel('virtual_network')`)

	if !r.Context.IsAdmin {
		query.Add(`.where(values('id_perms').select('user_visible').is(true))`)
		query.Add(`.where(
			or(
				__.out('parent').has(id, _tenant_id),
				has('router_external', true),
				has('is_shared', true)
			)
		)`)
		bindings["_tenant_id"] = r.Context.TenantID
	}

	// Add filters to the query
	filterQuery(query, bindings, r.Data.Filters,
		func(query *gremlinQuery, key string, valuesQuery string) {
			switch key {
			case "tenant_id":
				// Add this filter only in admin context, because in user context
				// the collection is already filtered above.
				if r.Context.IsAdmin {
					query.Addf(`.where(__.out('parent').has(id, %s))`, valuesQuery)
				}
			case "router_external":
				query.Addf(`.has('router_external', %s)`, valuesQuery)
			case "shared":
				query.Addf(`.has('is_shared', %s)`, valuesQuery)
			default:
				log.Warningf("No implementation for filter %s", key)
			}
		})

	valuesQuery(query, r.Data.Fields, networkDefaultFields,
		func(query *gremlinQuery, field string) {
			switch field {
			case "tenant_id":
				query.Add(`.by(__.out('parent').id().map{ it.get().toString().replace('-', '') })`)
			case "router_external":
				query.Add(`.by(
				coalesce(
					values('router_external'),
					constant(false)
				)
			)`)
			case "shared":
				query.Add(`.by(
				coalesce(
					values('is_shared'),
					constant(false)
				)
			)`)
			case "port_security_enabled":
				query.Add(`.by(
				coalesce(
					values('port_security_enabled'),
					constant(false)
				)
			)`)
			case "subnets":
				query.Add(`.by(
				coalesce(
					__.outE('ref').where(__.otherV().hasLabel('network_ipam'))
					  .values('ipam_subnets').unfold().select('subnet_uuid').fold(),
					constant([])
				)
			)`)
			case "status":
				query.Add(`.by(
				choose(
					values('id_perms').select('enable'),
					constant('ACTIVE'),
					constant('DOWN'),
				)
			)`)
			}
		})

	return app.execute(query, bindings)
}
