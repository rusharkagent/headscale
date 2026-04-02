package types

import (
	"net/netip"

	"gorm.io/gorm"
)

// TailnetID is the type for tailnet identifiers.
type TailnetID uint

// Tailnet represents a single isolated tailnet (virtual network).
// In a multi-tenant Headscale deployment, each customer gets their own
// Tailnet with isolated nodes, IPs, policies, and DNS.
type Tailnet struct {
	gorm.Model //nolint:embeddedstructfieldcheck

	// Name is a human-readable identifier for this tailnet (e.g. customer slug).
	Name string `gorm:"uniqueIndex"`

	// IPv4Prefix is the CGNAT range assigned to this tailnet (e.g. 100.64.0.0/10).
	IPv4Prefix netip.Prefix `gorm:"serializer:json"`

	// IPv6Prefix is the IPv6 range assigned to this tailnet (e.g. fd7a:115c:a1e0::/48).
	IPv6Prefix netip.Prefix `gorm:"serializer:json"`

	// BaseDomain is the MagicDNS base domain for this tailnet (e.g. example.ts.net).
	BaseDomain string

	// ACLPolicy holds the HuJSON ACL policy for this tailnet.
	// When empty, the global policy (from DB) is used as fallback.
	ACLPolicy string `gorm:"type:text"`
}
