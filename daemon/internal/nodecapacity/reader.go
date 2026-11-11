package nodecapacity

// Reader reads the node's capacity. Every call measures again.
type Reader interface {
	Read() (Capacity, error)
}
