package main

import (
	"fmt"
	"os"

	"github.com/anchore/stereoscope"
)

func main() {
	defer stereoscope.Cleanup()

	image, err := stereoscope.GetImage(os.Args[1], nil)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Manifest: %s\n\n", string(image.Metadata.RawManifest))
	fmt.Printf("Manifest digest: %s\n", image.Metadata.ManifestDigest)
}
