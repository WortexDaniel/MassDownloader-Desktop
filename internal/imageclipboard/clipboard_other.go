//go:build !windows

package imageclipboard

import "fmt"

func SetPNG(data []byte) error {
	return fmt.Errorf("буфер обмена изображений поддерживается только в Windows")
}
