package toml

import (
	"fmt"
	"reflect"

	"github.com/davecgh/go-spew/spew"
)

func dump(v reflect.Value) string {
	return fmt.Sprintf("* V:%s S:%t", spew.Sdump(v.Interface()), v.CanSet())
}

func init() {
	dump(reflect.ValueOf(""))
}
