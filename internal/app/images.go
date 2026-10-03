package app

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

const maxImageBytes = 10 << 20

type decodedImage struct {
	data    []byte
	mime    string
	dataURL string
}

// decodeDataURL validates a pasted image ("data:image/png;base64,...").
// The type is sniffed from the bytes, not trusted from the URL.
func decodeDataURL(u string) (decodedImage, error) {
	head, b64, ok := strings.Cut(u, ",")
	if !ok || !strings.HasPrefix(head, "data:image/") || !strings.HasSuffix(head, ";base64") {
		return decodedImage{}, errors.New("images must be sent as base64 data URLs")
	}
	if len(b64) > maxImageBytes*4/3+4 {
		return decodedImage{}, errors.New("image is too large (max 10 MB)")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return decodedImage{}, errors.New("image data is not valid base64")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
	default:
		return decodedImage{}, errors.New("unsupported image type " + mime + " (use PNG, JPEG, GIF, WebP or BMP)")
	}
	return decodedImage{data: data, mime: mime, dataURL: "data:" + mime + ";base64," + b64}, nil
}

// saveImage stores an image in the vault and returns its id.
func (a *App) saveImage(img decodedImage) (string, error) {
	v, err := a.unlocked()
	if err != nil {
		return "", err
	}
	id := newID()
	return id, v.Put("images/"+id, img.data)
}

// Image returns a stored image and its sniffed content type.
func (a *App) Image(id string) ([]byte, string, error) {
	v, err := a.unlocked()
	if err != nil {
		return nil, "", err
	}
	b, err := v.Get("images/" + id)
	if err != nil {
		return nil, "", err
	}
	return b, http.DetectContentType(b), nil
}
