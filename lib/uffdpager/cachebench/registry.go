package cachebench

// Factory builds a policy at a given byte budget and shard count. Policies that
// manage their own concurrency internally (prod, ristretto) ignore shardCount.
type Factory struct {
	Name    string
	Sharded bool
	New     func(maxBytes int64, shardCount int) Policy
}

func shardedFactory(name string, mk func(int64) shard) Factory {
	return Factory{
		Name:    name,
		Sharded: true,
		New: func(maxBytes int64, shardCount int) Policy {
			return newSharded(name, shardCount, maxBytes, mk)
		},
	}
}

// Factories returns every candidate policy in a stable order.
func Factories() []Factory {
	return []Factory{
		{Name: "prod", New: func(maxBytes int64, _ int) Policy { return newProd(maxBytes) }},
		shardedFactory("fifo", newFIFOShard),
		shardedFactory("lru", newLRUShard),
		shardedFactory("clock", newClockShard),
		shardedFactory("tinylfu", newTinyLFUShard),
		{Name: "ristretto", New: func(maxBytes int64, _ int) Policy { return newRistretto(maxBytes) }},
	}
}
