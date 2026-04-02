package db

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"sync"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/util"
	"github.com/rs/zerolog/log"
	"go4.org/netipx"
	"gorm.io/gorm"
	"tailscale.com/net/tsaddr"
)

var (
	errGeneratedIPBytesInvalid = errors.New("generated ip bytes are invalid ip")
	errGeneratedIPNotInPrefix  = errors.New("generated ip not in prefix")
	errIPAllocatorNil          = errors.New("ip allocator was nil")
)

// IPAllocator is a singleton responsible for allocating
// IP addresses for nodes and making sure the same
// address is not handed out twice. There can only be one
// and it needs to be created before any other database
// writes occur.
type IPAllocator struct {
	mu sync.Mutex

	prefix4 *netip.Prefix
	prefix6 *netip.Prefix

	// Previous IPs handed out
	prev4 netip.Addr
	prev6 netip.Addr

	// strategy used for handing out IP addresses.
	strategy types.IPAllocationStrategy

	// Set of all IPs handed out.
	// This might not be in sync with the database,
	// but it is more conservative. If saves to the
	// database fails, the IP will be allocated here
	// until the next restart of Headscale.
	usedIPs netipx.IPSetBuilder
}

// NewIPAllocator returns a new IPAllocator singleton which
// can be used to hand out unique IP addresses within the
// provided IPv4 and IPv6 prefix. It needs to be created
// when headscale starts and needs to finish its read
// transaction before any writes to the database occur.
func NewIPAllocator(
	db *HSDatabase,
	prefix4, prefix6 *netip.Prefix,
	strategy types.IPAllocationStrategy,
) (*IPAllocator, error) {
	ret := IPAllocator{
		prefix4: prefix4,
		prefix6: prefix6,

		strategy: strategy,
	}

	var (
		v4s []sql.NullString
		v6s []sql.NullString
	)

	if db != nil {
		err := db.Read(func(rx *gorm.DB) error {
			return rx.Model(&types.Node{}).Pluck("ipv4", &v4s).Error
		})
		if err != nil {
			return nil, fmt.Errorf("reading IPv4 addresses from database: %w", err)
		}

		err = db.Read(func(rx *gorm.DB) error {
			return rx.Model(&types.Node{}).Pluck("ipv6", &v6s).Error
		})
		if err != nil {
			return nil, fmt.Errorf("reading IPv6 addresses from database: %w", err)
		}
	}

	var ips netipx.IPSetBuilder

	// Add network and broadcast addrs to used pool so they
	// are not handed out to nodes.
	if prefix4 != nil {
		network4, broadcast4 := util.GetIPPrefixEndpoints(*prefix4)
		ips.Add(network4)
		ips.Add(broadcast4)

		// Use network as starting point, it will be used to call .Next()
		// TODO(kradalby): Could potentially take all the IPs loaded from
		// the database into account to start at a more "educated" location.
		ret.prev4 = network4
	}

	if prefix6 != nil {
		network6, broadcast6 := util.GetIPPrefixEndpoints(*prefix6)
		ips.Add(network6)
		ips.Add(broadcast6)

		ret.prev6 = network6
	}

	// Fetch all the IP Addresses currently handed out from the Database
	// and add them to the used IP set.
	for _, addrStr := range append(v4s, v6s...) {
		if addrStr.Valid {
			addr, err := netip.ParseAddr(addrStr.String)
			if err != nil {
				return nil, fmt.Errorf("parsing IP address from database: %w", err)
			}

			ips.Add(addr)
		}
	}

	// Build the initial IPSet to validate that we can use it.
	_, err := ips.IPSet()
	if err != nil {
		return nil, fmt.Errorf(
			"building initial IP Set: %w",
			err,
		)
	}

	ret.usedIPs = ips

	return &ret, nil
}

func (i *IPAllocator) Next() (*netip.Addr, *netip.Addr, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	var (
		err  error
		ret4 *netip.Addr
		ret6 *netip.Addr
	)

	if i.prefix4 != nil {
		ret4, err = i.next(i.prev4, i.prefix4)
		if err != nil {
			return nil, nil, fmt.Errorf("allocating IPv4 address: %w", err)
		}

		i.prev4 = *ret4
	}

	if i.prefix6 != nil {
		ret6, err = i.next(i.prev6, i.prefix6)
		if err != nil {
			return nil, nil, fmt.Errorf("allocating IPv6 address: %w", err)
		}

		i.prev6 = *ret6
	}

	return ret4, ret6, nil
}

var ErrCouldNotAllocateIP = errors.New("failed to allocate IP")

func (i *IPAllocator) nextLocked(prev netip.Addr, prefix *netip.Prefix) (*netip.Addr, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.next(prev, prefix)
}

func (i *IPAllocator) next(prev netip.Addr, prefix *netip.Prefix) (*netip.Addr, error) {
	var (
		err error
		ip  netip.Addr
	)

	switch i.strategy {
	case types.IPAllocationStrategySequential:
		// Get the first IP in our prefix
		ip = prev.Next()
	case types.IPAllocationStrategyRandom:
		ip, err = randomNext(*prefix)
		if err != nil {
			return nil, fmt.Errorf("getting random IP: %w", err)
		}
	}

	// TODO(kradalby): maybe this can be done less often.
	set, err := i.usedIPs.IPSet()
	if err != nil {
		return nil, err
	}

	for {
		if !prefix.Contains(ip) {
			return nil, ErrCouldNotAllocateIP
		}

		// Check if the IP has already been allocated
		// or if it is a IP reserved by Tailscale.
		if set.Contains(ip) || isTailscaleReservedIP(ip) {
			switch i.strategy {
			case types.IPAllocationStrategySequential:
				ip = ip.Next()
			case types.IPAllocationStrategyRandom:
				ip, err = randomNext(*prefix)
				if err != nil {
					return nil, fmt.Errorf("getting random IP: %w", err)
				}
			}

			continue
		}

		i.usedIPs.Add(ip)

		return &ip, nil
	}
}

func randomNext(pfx netip.Prefix) (netip.Addr, error) {
	rang := netipx.RangeOfPrefix(pfx)
	fromIP, toIP := rang.From(), rang.To()

	var from, to big.Int

	from.SetBytes(fromIP.AsSlice())
	to.SetBytes(toIP.AsSlice())

	// Find the max, this is how we can do "random range",
	// get the "max" as 0 -> to - from and then add back from
	// after.
	tempMax := big.NewInt(0).Sub(&to, &from)

	out, err := rand.Int(rand.Reader, tempMax)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("generating random IP: %w", err)
	}

	valInRange := big.NewInt(0).Add(&from, out)

	ip, ok := netip.AddrFromSlice(valInRange.Bytes())
	if !ok {
		return netip.Addr{}, errGeneratedIPBytesInvalid
	}

	if !pfx.Contains(ip) {
		return netip.Addr{}, fmt.Errorf(
			"%w: ip(%s) not in prefix(%s)",
			errGeneratedIPNotInPrefix,
			ip.String(),
			pfx.String(),
		)
	}

	return ip, nil
}

func isTailscaleReservedIP(ip netip.Addr) bool {
	return tsaddr.ChromeOSVMRange().Contains(ip) ||
		tsaddr.TailscaleServiceIP() == ip ||
		tsaddr.TailscaleServiceIPv6() == ip
}

// BackfillNodeIPs will take a database transaction, and
// iterate through all of the current nodes in headscale
// and ensure it has IP addresses according to the current
// configuration.
// This means that if both IPv4 and IPv6 is set in the
// config, and some nodes are missing that type of IP,
// it will be added.
// If a prefix type has been removed (IPv4 or IPv6), it
// will remove the IPs in that family from the node.
func (db *HSDatabase) BackfillNodeIPs(i *IPAllocator) ([]string, error) {
	var (
		err error
		ret []string
	)

	err = db.Write(func(tx *gorm.DB) error {
		if i == nil {
			return fmt.Errorf("backfilling IPs: %w", errIPAllocatorNil)
		}

		log.Trace().Caller().Msgf("starting to backfill IPs")

		nodes, err := ListNodes(tx)
		if err != nil {
			return fmt.Errorf("listing nodes to backfill IPs: %w", err)
		}

		for _, node := range nodes {
			log.Trace().Caller().EmbedObject(node).Msg("ip backfill check started because node found in database")

			changed := false
			// IPv4 prefix is set, but node ip is missing, alloc
			if i.prefix4 != nil && node.IPv4 == nil {
				ret4, err := i.nextLocked(i.prev4, i.prefix4)
				if err != nil {
					return fmt.Errorf("allocating IPv4 for node(%d): %w", node.ID, err)
				}

				node.IPv4 = ret4
				changed = true

				ret = append(ret, fmt.Sprintf("assigned IPv4 %q to Node(%d) %q", ret4.String(), node.ID, node.Hostname))
			}

			// IPv6 prefix is set, but node ip is missing, alloc
			if i.prefix6 != nil && node.IPv6 == nil {
				ret6, err := i.nextLocked(i.prev6, i.prefix6)
				if err != nil {
					return fmt.Errorf("allocating IPv6 for node(%d): %w", node.ID, err)
				}

				node.IPv6 = ret6
				changed = true

				ret = append(ret, fmt.Sprintf("assigned IPv6 %q to Node(%d) %q", ret6.String(), node.ID, node.Hostname))
			}

			// IPv4 prefix is not set, but node has IP, remove
			if i.prefix4 == nil && node.IPv4 != nil {
				ret = append(ret, fmt.Sprintf("removing IPv4 %q from Node(%d) %q", node.IPv4.String(), node.ID, node.Hostname))
				node.IPv4 = nil
				changed = true
			}

			// IPv6 prefix is not set, but node has IP, remove
			if i.prefix6 == nil && node.IPv6 != nil {
				ret = append(ret, fmt.Sprintf("removing IPv6 %q from Node(%d) %q", node.IPv6.String(), node.ID, node.Hostname))
				node.IPv6 = nil
				changed = true
			}

			if changed {
				// Use Updates() with Select() to only update IP fields, avoiding overwriting
				// other fields like Expiry. We need Select() because Updates() alone skips
				// zero values, but we DO want to update IPv4/IPv6 to nil when removing them.
				// See issue #2862.
				err := tx.Model(node).Select("ipv4", "ipv6").Updates(node).Error
				if err != nil {
					return fmt.Errorf("saving node(%d) after adding IPs: %w", node.ID, err)
				}
			}
		}

		return nil
	})

	return ret, err
}

func (i *IPAllocator) FreeIPs(ips []netip.Addr) {
	i.mu.Lock()
	defer i.mu.Unlock()

	for _, ip := range ips {
		i.usedIPs.Remove(ip)
	}
}

// ---------------------------------------------------------------------------
// TailnetIPAllocator — per-tailnet IP allocation
// ---------------------------------------------------------------------------

// TailnetIPAllocator manages a separate IPAllocator per tailnet.
// This ensures that nodes in different tailnets get IPs from their own
// dedicated prefix and can never accidentally share or conflict on addresses.
//
// tailnetID=0 is the "default" tailnet — used for nodes with a NULL tailnet_id
// and for backward compatibility with single-tenant deployments.
type TailnetIPAllocator struct {
	mu         sync.RWMutex
	allocators map[uint]*IPAllocator // tailnetID → allocator (0 = default)
	strategy   types.IPAllocationStrategy
}

// NewTailnetIPAllocator builds a TailnetIPAllocator by:
//  1. Creating a default allocator for tailnetID=0 using cfg.PrefixV4/V6
//     (backward compat — nodes without a tailnet_id use this pool).
//  2. Loading all tailnets from the DB and creating a scoped allocator for each.
func NewTailnetIPAllocator(
	db *HSDatabase,
	defaultPrefix4, defaultPrefix6 *netip.Prefix,
	strategy types.IPAllocationStrategy,
) (*TailnetIPAllocator, error) {
	ta := &TailnetIPAllocator{
		allocators: make(map[uint]*IPAllocator),
		strategy:   strategy,
	}

	// Default tailnet (id=0): uses the global config prefix.
	// This is the backward-compat path for existing single-tenant deployments.
	defaultAlloc, err := newIPAllocatorForTailnet(db, 0, defaultPrefix4, defaultPrefix6, strategy)
	if err != nil {
		return nil, fmt.Errorf("creating default IP allocator: %w", err)
	}

	ta.allocators[0] = defaultAlloc

	// Per-tailnet allocators from the DB.
	if db != nil {
		tailnets, err := db.ListTailnets()
		if err != nil {
			return nil, fmt.Errorf("loading tailnets for IP allocator: %w", err)
		}

		for _, tn := range tailnets {
			if !tn.IPv4Prefix.IsValid() && !tn.IPv6Prefix.IsValid() {
				// Tailnet has no prefix yet — skip (will use default).
				continue
			}

			var p4, p6 *netip.Prefix
			if tn.IPv4Prefix.IsValid() {
				p := tn.IPv4Prefix
				p4 = &p
			}

			if tn.IPv6Prefix.IsValid() {
				p := tn.IPv6Prefix
				p6 = &p
			}

			alloc, err := newIPAllocatorForTailnet(db, tn.ID, p4, p6, strategy)
			if err != nil {
				return nil, fmt.Errorf("creating IP allocator for tailnet %d (%q): %w", tn.ID, tn.Name, err)
			}

			ta.allocators[tn.ID] = alloc
		}
	}

	return ta, nil
}

// newIPAllocatorForTailnet creates an IPAllocator scoped to a specific tailnet,
// pre-loading only the IPs already used by nodes in that tailnet.
func newIPAllocatorForTailnet(
	db *HSDatabase,
	tailnetID uint,
	prefix4, prefix6 *netip.Prefix,
	strategy types.IPAllocationStrategy,
) (*IPAllocator, error) {
	alloc := &IPAllocator{
		prefix4:  prefix4,
		prefix6:  prefix6,
		strategy: strategy,
	}

	var ips netipx.IPSetBuilder

	if prefix4 != nil {
		network4, broadcast4 := util.GetIPPrefixEndpoints(*prefix4)
		ips.Add(network4)
		ips.Add(broadcast4)
		alloc.prev4 = network4
	}

	if prefix6 != nil {
		network6, broadcast6 := util.GetIPPrefixEndpoints(*prefix6)
		ips.Add(network6)
		ips.Add(broadcast6)
		alloc.prev6 = network6
	}

	// Load used IPs from DB, scoped to this tailnet.
	if db != nil {
		var v4s, v6s []sql.NullString

		err := db.Read(func(rx *gorm.DB) error {
			q := rx.Model(&types.Node{})
			if tailnetID == 0 {
				q = q.Where("tailnet_id IS NULL OR tailnet_id = 0")
			} else {
				q = q.Where("tailnet_id = ?", tailnetID)
			}

			if err := q.Pluck("ipv4", &v4s).Error; err != nil {
				return err
			}

			q2 := rx.Model(&types.Node{})
			if tailnetID == 0 {
				q2 = q2.Where("tailnet_id IS NULL OR tailnet_id = 0")
			} else {
				q2 = q2.Where("tailnet_id = ?", tailnetID)
			}

			return q2.Pluck("ipv6", &v6s).Error
		})
		if err != nil {
			return nil, fmt.Errorf("reading used IPs for tailnet %d: %w", tailnetID, err)
		}

		for _, addrStr := range append(v4s, v6s...) {
			if addrStr.Valid {
				addr, err := netip.ParseAddr(addrStr.String)
				if err != nil {
					return nil, fmt.Errorf("parsing IP for tailnet %d: %w", tailnetID, err)
				}

				ips.Add(addr)
			}
		}
	}

	if _, err := ips.IPSet(); err != nil {
		return nil, fmt.Errorf("building IP set for tailnet %d: %w", tailnetID, err)
	}

	alloc.usedIPs = ips

	return alloc, nil
}

// Next allocates the next available IPv4/IPv6 pair for the given tailnet.
// Falls back to tailnetID=0 if no dedicated allocator exists.
func (ta *TailnetIPAllocator) Next(tailnetID uint) (*netip.Addr, *netip.Addr, error) {
	ta.mu.RLock()
	alloc, ok := ta.allocators[tailnetID]
	ta.mu.RUnlock()

	if !ok {
		// Fall back to default allocator for unknown tailnets.
		ta.mu.RLock()
		alloc = ta.allocators[0]
		ta.mu.RUnlock()
	}

	return alloc.Next()
}

// FreeIPs releases the given IPs back to the tailnet's pool.
func (ta *TailnetIPAllocator) FreeIPs(tailnetID uint, ips []netip.Addr) {
	ta.mu.RLock()
	alloc, ok := ta.allocators[tailnetID]
	ta.mu.RUnlock()

	if !ok {
		ta.mu.RLock()
		alloc = ta.allocators[0]
		ta.mu.RUnlock()
	}

	alloc.FreeIPs(ips)
}

// RegisterTailnet adds (or replaces) the allocator for a new tailnet.
// Called when a new tailnet is created at runtime.
func (ta *TailnetIPAllocator) RegisterTailnet(
	db *HSDatabase,
	tailnetID uint,
	prefix4, prefix6 *netip.Prefix,
) error {
	alloc, err := newIPAllocatorForTailnet(db, tailnetID, prefix4, prefix6, ta.strategy)
	if err != nil {
		return err
	}

	ta.mu.Lock()
	ta.allocators[tailnetID] = alloc
	ta.mu.Unlock()

	return nil
}

// BackfillNodeIPsMultiTenant assigns IPs to nodes that are missing them,
// using each node's tailnet allocator.
func (db *HSDatabase) BackfillNodeIPsMultiTenant(ta *TailnetIPAllocator) ([]string, error) {
	var ret []string

	err := db.Write(func(tx *gorm.DB) error {
		if ta == nil {
			return fmt.Errorf("backfilling IPs: allocator is nil")
		}

		nodes, err := ListNodes(tx)
		if err != nil {
			return fmt.Errorf("listing nodes to backfill IPs: %w", err)
		}

		for _, node := range nodes {
			tailnetID := uint(0)
			if node.TailnetID != nil {
				tailnetID = *node.TailnetID
			}

			ta.mu.RLock()
			alloc, ok := ta.allocators[tailnetID]
			if !ok {
				alloc = ta.allocators[0]
			}
			ta.mu.RUnlock()

			changed := false

			if alloc.prefix4 != nil && node.IPv4 == nil {
				ret4, err := alloc.nextLocked(alloc.prev4, alloc.prefix4)
				if err != nil {
					return fmt.Errorf("allocating IPv4 for node(%d) in tailnet(%d): %w", node.ID, tailnetID, err)
				}

				node.IPv4 = ret4
				changed = true
				ret = append(ret, fmt.Sprintf("assigned IPv4 %q to Node(%d) %q (tailnet %d)", ret4, node.ID, node.Hostname, tailnetID))
			}

			if alloc.prefix6 != nil && node.IPv6 == nil {
				ret6, err := alloc.nextLocked(alloc.prev6, alloc.prefix6)
				if err != nil {
					return fmt.Errorf("allocating IPv6 for node(%d) in tailnet(%d): %w", node.ID, tailnetID, err)
				}

				node.IPv6 = ret6
				changed = true
				ret = append(ret, fmt.Sprintf("assigned IPv6 %q to Node(%d) %q (tailnet %d)", ret6, node.ID, node.Hostname, tailnetID))
			}

			if alloc.prefix4 == nil && node.IPv4 != nil {
				ret = append(ret, fmt.Sprintf("removing IPv4 %q from Node(%d) %q (tailnet %d)", node.IPv4, node.ID, node.Hostname, tailnetID))
				node.IPv4 = nil
				changed = true
			}

			if alloc.prefix6 == nil && node.IPv6 != nil {
				ret = append(ret, fmt.Sprintf("removing IPv6 %q from Node(%d) %q (tailnet %d)", node.IPv6, node.ID, node.Hostname, tailnetID))
				node.IPv6 = nil
				changed = true
			}

			if changed {
				err := tx.Model(node).Select("ipv4", "ipv6").Updates(node).Error
				if err != nil {
					return fmt.Errorf("saving node(%d) after IP backfill: %w", node.ID, err)
				}
			}
		}

		return nil
	})

	return ret, err
}
