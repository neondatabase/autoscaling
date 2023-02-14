package informant

// Integration with Neon's postgres local file cache

import (
	"context"
	// "database/sql" //nolint:gocritic
	// _ "github.com/lib/pq"
	"fmt"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type FileCacheState struct {
	connStr string
	config  FileCacheConfig
}

type FileCacheConfig struct {
	// InMemory indicates whether the file cache is *actually* stored in memory (e.g. by writing to
	// a tmpfs or shmem file). If true, the size of the file cache will be counted against the
	// memory available for the cgroup.
	InMemory bool

	// ResourceMultiplier gives the size of the file cache, in terms of the size of the resource it
	// consumes (currently: only memory)
	//
	// For example, setting ResourceMultiplier = 0.75 gives the cache a target size of 75% of total
	// resources.
	//
	// This value must be strictly between 0 and 1.
	ResourceMultiplier float64

	// MinRemainingAfterCache gives the required minimum amount of memory, in bytes, that must
	// remain available after subtracting the file cache.
	//
	// This value must be non-zero.
	MinRemainingAfterCache uint64

	// SpreadFactor controls the rate of increase in the file cache's size as it grows from zero
	// (when total resources equals MinRemainingAfterCache) to the desired size based on
	// ResourceMultiplier.
	//
	// A SpreadFactor of zero means that all additional resources will go to the cache until it
	// reaches the desired size. Setting SpreadFactor to N roughly means "for every 1 byte added to
	// the cache's size, N bytes are reserved for the rest of the system, until the cache gets to
	// its desired size".
	//
	// This value must be >= 0, and must retain an increase that is more than what would be given by
	// ResourceMultiplier. For example, setting ResourceMultiplier = 0.75 but SpreadFactor = 1 would
	// be invalid, because SpreadFactor would induce only 50% usage - never reaching the 75% as
	// desired by ResourceMultiplier.
	//
	// SpreadFactor is too large if (SpreadFactor+1) * ResourceMultiplier is >= 1.
	SpreadFactor float64
}

func (c *FileCacheConfig) Validate() error {
	// Check single-field validity
	if !(0.0 < c.ResourceMultiplier && c.ResourceMultiplier < 1.0) {
		return fmt.Errorf("ResourceMultiplier must be between 0.0 and 1.0, exclusive. Got %g", c.ResourceMultiplier)
	} else if !(c.SpreadFactor >= 0.0) {
		return fmt.Errorf("SpreadFactor must be >= 0, got: %g", c.SpreadFactor)
	} else if c.MinRemainingAfterCache == 0 {
		return fmt.Errorf("MinRemainingAfterCache must not be 0")
	}

	// Check that ResourceMultiplier and SpreadFactor are valid w.r.t. each other.
	//
	// As shown in CalculateCacheSize, we have two lines resulting from ResourceMultiplier and
	// SpreadFactor, respectively. They are:
	//
	//                total           MinRemainingAfterCache
	//   size = —————————————————— - ————————————————————————
	//           SpreadFactor + 1        SpreadFactor + 1
	//
	// and
	//
	//   size = ResourceMultiplier × total
	//
	// .. where 'total' is the total resources. These are isomorphic to the typical 'y = mx + b'
	// form, with y = "size" and x = "total".
	//
	// These lines intersect at:
	//
	//               MinRemainingAfterCache
	//   —————————————————————————————————————————————
	//    1 - ResourceMultiplier × (SpreadFactor + 1)
	//
	// We want to ensure that this value (a) exists, and (b) is >= MinRemainingAfterCache. This is
	// guaranteed when 'ResourceMultiplier × (SpreadFactor + 1)' is less than 1.
	// (We also need it to be >= 0, but that's already guaranteed.)

	intersectFactor := c.ResourceMultiplier * (c.SpreadFactor + 1)
	if !(intersectFactor < 1.0) {
		return fmt.Errorf("incompatible ResourceMultiplier and SpreadFactor")
	}

	return nil
}

// CalculateCacheSize returns the desired size of the cache, given the total memory.
func (c *FileCacheConfig) CalculateCacheSize(total uint64) uint64 {
	available := util.SaturatingSub(total, c.MinRemainingAfterCache)

	if available <= c.MinRemainingAfterCache {
		return 0
	}

	sizeFromSpread := uint64(util.Max(0, int64(float64(available)/(1.0+c.SpreadFactor))))
	//                ^^^^^^^^^^^^^^^^^^^^^^^^ make sure we don't overflow from floating-point ops
	sizeFromNormal := uint64(float64(total) * c.ResourceMultiplier)

	byteSize := util.Min(sizeFromSpread, sizeFromNormal)
	var mib uint64 = 1 << 20 // 1 MiB = 1^20 bytes.

	// The file cache operates in units of mebibytes, so the sizes we produce should be rounded to a
	// mebibyte. We round down to be conservative.
	return byteSize / mib * mib
}

// DO NOT MERGE: this implementation is just for immediate testing, without the file cache
// available on staging.
func (s *FileCacheState) GetFileCacheSize(ctx context.Context) (uint64, error) {
	return 9999, nil
}

// DO NOT MERGE: Same as above.
func (s *FileCacheState) SetFileCacheSize(ctx context.Context, sizeInBytes uint64) (uint64, error) {
	sizeInMB := sizeInBytes / (1 << 20)
	klog.Infof("Updating file cache size to %d MiB", sizeInMB)
	return sizeInMB * (1 << 20), nil
}

// // GetFileCacheSize returns the current size of the file cache, in bytes
// func (s *FileCacheState) GetFileCacheSize(ctx context.Context) (uint64, error) {
// 	db, err := sql.Open("postgres", s.connStr)
// 	if err != nil {
// 		return 0, fmt.Errorf("Error connecting to postgres: %w", err)
// 	}
//
// 	var sizeInMB uint64
// 	if err := db.QueryRowContext(ctx, "SHOW neon.file_cache_size_limit").Scan(&sizeInMB); err != nil {
// 		return 0, fmt.Errorf("Error querying file cache size: %w", err)
// 	}
//
// 	// The file cache GUC variable is in MiB
// 	return sizeInMB * (1 << 20), nil
// }
//
// // SetFileCacheSize sets the size of the file cache, returning the actual size it was set to
// func (s *FileCacheState) SetFileCacheSize(ctx context.Context, sizeInBytes uint64) (uint64, error) {
// 	db, err := sql.Open("postgres", s.connStr)
// 	if err != nil {
// 		return 0, fmt.Errorf("Error connecting to postgres: %w", err)
// 	}
//
// 	sizeInMB := sizeInBytes / (1 << 20)
//  klog.Infof("Updating file cache size to %d MiB", sizeInMB)
//
// 	// TODO: query should cap the value with size neon.max_file_cache_size, then return the actual
// 	// size we set it to.
// 	if _, err := db.ExecContext(ctx, "SET neon.file_cache_size_limit = $1", sizeInMB); err != nil {
// 		return 0, fmt.Errorf("Error setting file cache size: %w", err)
// 	}
//
// 	return sizeInMB * (1 << 20), nil
// }
