package xact

// Xact represents a single in-memory transaction, to aid with separating calculations from their
// application.
type Xact[T any] struct {
	tmp  T
	base *T
}

// New returns a new transaction object (called Xact) operating on the given pointer
//
// NOTE: Any copying is shallow -- if T contains pointers, any changes to the values behind those
// will NOT be delayed until (*Xact[T]).Commit().
func New[T any](ptr *T) *Xact[T] {
	return &Xact[T]{
		tmp:  *ptr,
		base: ptr,
	}
}

// Value returns a pointer to the temporary value stored in the Xact
//
// The returned value can be freely modified; it will have no effect until the transaction is
// committed with Commit().
func (x *Xact[T]) Value() *T {
	return &x.tmp
}

// Commit assigns the temporary value back to the original pointer that the Xact was created with
//
// A transaction can be committed multiple times, if it's useful to reuse it.
func (x *Xact[T]) Commit() {
	*x.base = x.tmp
}
