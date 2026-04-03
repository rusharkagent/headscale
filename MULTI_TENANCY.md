# Multi-Tenancy in Headscale

This document describes the multi-tenancy architecture implemented in this fork of [juanfont/headscale](https://github.com/juanfont/headscale). It explains the problem, the design decisions, and how to operate a multi-tenant deployment.

---

## Background

Upstream Headscale assumes a single global tailnet. Every node, user, pre-auth key, IP address, ACL policy, and DNS domain is shared across the entire instance. This makes it unsuitable for SaaS or managed deployments where multiple customers need full isolation.

The changes in this branch layer multi-tenancy on top of the existing codebase with a minimal footprint: a new `Tailnet` model becomes the isolation boundary, and a `tailnet_id` foreign key is threaded through the relevant tables.

---

## Architecture

### The Isolation Boundary: `Tailnet`

A `Tailnet` represents one customer's virtual network. It owns:

| Resource | Scoped to Tailnet |
|---|---|
| Nodes | ✅ via `tailnet_id` FK |
| Users | ✅ via `tailnet_id` FK |
| Pre-auth keys | ✅ via `tailnet_id` FK |
| IP address pool | ✅ per-tailnet `IPAllocator` |
| Peer visibility | ✅ peer map built per-tailnet |
| ACL policy | ✅ per-tailnet `PolicyManager` |
| MagicDNS domain | ✅ per-tailnet `BaseDomain` |

### Data Model

```go
type Tailnet struct {
    gorm.Model
    Name       string       // unique slug, e.g. "acme"
    IPv4Prefix netip.Prefix // e.g. 100.64.0.0/16
    IPv6Prefix netip.Prefix // e.g. fd7a:115c:a1e0::/48
    BaseDomain string       // e.g. acme.ts.net
    ACLPolicy  string       // HuJSON ACL stored in DB
}
```

`Node`, `User`, and `PreAuthKey` each have a `TailnetID *uint` foreign key. **This field must always be set** — there is no default or fallback tailnet. Every resource must belong to an explicit `Tailnet`.

### Isolation Layers (5 phases)

#### Phase 1 — Schema

- New `tailnets` table with prefix + domain + ACL fields
- `tailnet_id` column added to `nodes`, `users`, `pre_auth_keys`
- Migration `202604020000-multi-tenancy-tailnet` adds the FK columns — no default tailnet is seeded; every record must be assigned to an explicit tailnet at creation time
- `schema.sql` updated as the squibble validation source of truth

#### Phase 2 — Peer Map Scoping

- `NodeStore.Snapshot` now indexes nodes by tailnet (`nodesByTailnet`)
- The `PeersFunc` groups all nodes by tailnet before calling `BuildPeerMap` — nodes in different tailnets are **structurally prevented** from appearing in each other's peer maps, regardless of ACL
- `State.ListPeers(nodeID, peerIDs...)` resolves candidates only within the requesting node's tailnet

#### Phase 3 — Per-Tailnet IP Allocation

- `TailnetIPAllocator` replaces the single global `IPAllocator`
- On startup it loads all tailnets and creates a scoped `IPAllocator` for each, pre-loading only the IPs already used by nodes in that tailnet
- New nodes get IPs from their tailnet's pool; deleted nodes return IPs to the same pool
- Allocating IPs for an unknown tailnet returns an error — there is no fallback pool
- `RegisterTailnet()` adds a pool at runtime when a new tailnet is created

#### Phase 4 — Per-Tailnet Policy and DNS

**ACL Policy:**
- `State` holds a `perTailnetPolMan map[uint]PolicyManager`
- Tailnets with a non-empty `ACLPolicy` get a dedicated `PolicyManager` initialised from their stored policy and scoped to their users/nodes
- All node-context policy calls (`FilterForNode`, `MatchersForNode`, `SSHPolicy`, `NodeCanHaveTag`, `ViaRoutesForPeer`, route auto-approval) route through `getPolManForNode` → falls back to the global manager when no override exists
- `RegisterTailnetPolicy(id, policy)` hot-reloads a tailnet's ACL without restart

**DNS:**
- `State.BaseDomainForNode(node)` resolves the effective MagicDNS domain for a node using this priority order:
  1. Explicit `Tailnet.BaseDomain` override (operator-set)
  2. Auto-derived: `<tailnet-name>.<global-base-domain>` — single config, automatic namespacing per tenant
  3. Global `dns.base_domain` as fallback (e.g. if tailnet has no name)
- This means **no per-tenant DNS config is needed**. Set `dns.base_domain = ts.example.com` once; nodes in tailnet `acme` automatically get FQDNs like `laptop.acme.ts.example.com`. Auth determines the tailnet; the tailnet determines the subdomain.
- The mapper's `cfgForNode(node)` creates a shallow `Config` copy with `BaseDomain` overridden — no allocation when the domain is unchanged
- `TailNode()` (which generates FQDNs) receives the node-specific config, producing correct per-tenant domains

#### Phase 5 — Management API and CLI

REST API at `/api/v1/tailnet` (protected by the existing API key middleware):

| Method | Path | Action |
|---|---|---|
| `GET` | `/api/v1/tailnet` | List all tailnets |
| `POST` | `/api/v1/tailnet` | Create a tailnet |
| `GET` | `/api/v1/tailnet/{id}` | Get a tailnet |
| `PUT` | `/api/v1/tailnet/{id}` | Update base domain / ACL |
| `DELETE` | `/api/v1/tailnet/{id}` | Delete a tailnet |
| `PUT` | `/api/v1/tailnet/{id}/policy` | Set ACL policy (hot-reload) |

CLI subcommand `headscale tailnets` (calls the REST API using the configured `cli.address` + `cli.api_key`):

```
headscale tailnets list
headscale tailnets create <name> [--ipv4-prefix] [--ipv6-prefix] [--base-domain] [--policy-file]
headscale tailnets get <id>
headscale tailnets update <id> [--base-domain] [--policy-file]
headscale tailnets delete <id> [--force]
headscale tailnets set-policy <id> --policy-file <path>
```

---

## Operating a Multi-Tenant Deployment

> **Note:** There is no default tailnet. Every node, user, and pre-auth key must belong to an explicit tenant created via the API or CLI. Single-tenant mode is not supported — create a named tailnet for your single customer if needed.

### 1. Configure a single base domain (one-time, in headscale config)

```yaml
# config.yaml
dns:
  base_domain: ts.example.com   # all tenants share this — no per-tenant DNS config needed
```

Node FQDNs are auto-namespaced from the tailnet name:
- Tailnet `acme` → `laptop.acme.ts.example.com`
- Tailnet `corp` → `laptop.corp.ts.example.com`

Auth (pre-auth key or OIDC) determines which tailnet a node joins. The tailnet determines the subdomain prefix. You don't configure DNS per tenant.

### 2. Create a tenant

```bash
# CLI — no --base-domain needed, derived automatically from tailnet name
headscale tailnets create acme \
  --ipv4-prefix 100.64.0.0/16 \
  --ipv6-prefix fd7a:115c:a1e0::/48 \
  --policy-file /etc/headscale/policies/acme.hujson

# To override the domain explicitly (optional):
headscale tailnets create acme \
  --ipv4-prefix 100.64.0.0/16 \
  --base-domain custom.acme.net   # overrides the auto-derived domain

# REST
curl -X POST https://headscale.example.com/api/v1/tailnet \
  -H "Authorization: Bearer <apikey>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "acme",
    "ipv4_prefix": "100.64.0.0/16",
    "ipv6_prefix": "fd7a:115c:a1e0::/48",
    "acl_policy": "{\"action\": \"accept\", ...}"
  }'
```

### 3. Create users inside the tailnet  

At present, user creation uses the existing `headscale users create` command. The `tailnet_id` is assigned by setting it on the user record directly via the API or future CLI flag. This is a known gap — user creation with explicit tailnet assignment is the next increment.

### 4. Create pre-auth keys for the tailnet

Pre-auth keys inherit their tailnet from the owning user's `tailnet_id`. Nodes registered with a key inherit the same tailnet.

### 5. Update a tailnet's ACL (hot-reload)

```bash
headscale tailnets set-policy 2 --policy-file /etc/headscale/policies/acme-v2.hujson
```

This persists the new policy to the DB and immediately reloads the in-memory `PolicyManager` and rebuilds peer maps — no restart required.

### 6. Update the MagicDNS domain

```bash
headscale tailnets update 2 --base-domain new.acme.ts.net
```

Takes effect for the next MapResponse sent to each node.

---

## IP Prefix Planning

Each tailnet needs its own non-overlapping prefix. The full CGNAT range is `100.64.0.0/10` (~4M addresses). Example split for up to 256 tenants:

| Tailnet | IPv4 Prefix |
|---|---|
| tenant-1 | `100.64.0.0/18` (16k addresses) |
| tenant-2 | `100.64.64.0/18` |
| tenant-3 | `100.64.128.0/18` |
| ... | ... |

IPv6: use per-tenant `/64` subnets under `fd7a:115c:a1e0::/48`.

---

## Design Decisions

**Why a `tailnet_id` FK rather than separate DB schemas or separate Headscale instances?**

Separate instances is operationally expensive and doesn't share the DERP map, noise key infrastructure, or binary. A FK column is the minimal change that achieves isolation — easy to add, easy to query, easy to migrate.

**Why keep a global `polMan` at all?**

Tailnets without a stored ACL policy fall back to the global policy manager (loaded from file or DB). This acts as a shared baseline — useful for applying a common "deny all" or "allow same-tailnet" rule without duplicating it per tenant. Per-tailnet policy managers are opt-in overrides.

**Why a plain HTTP REST API instead of extending the gRPC proto?**

Extending the proto requires regenerating the gRPC-gateway bindings with `buf`, which adds toolchain complexity. The chi router already handles non-gRPC routes. The REST API uses the same auth middleware and is functionally equivalent. Proto definitions can be added in a follow-up once the API surface stabilises.

---

## Known Gaps and Next Steps

| Area | Status | Notes |
|---|---|---|
| User creation with `--tailnet` flag | 🔲 | Currently requires direct DB or API |
| Pre-auth key scoping via CLI | 🔲 | Key inherits from user for now |
| Tailnet listing in `headscale nodes list` | 🔲 | Should filter by tailnet |
| Per-tailnet DERP map | 🔲 | Currently shared globally |
| Tailnet usage metrics | 🔲 | Prometheus labels per tailnet |
| Integration tests | 🔲 | Node isolation assertions |

---

## Linear

Tracked under **TRE-42** in the Trescale workspace.
