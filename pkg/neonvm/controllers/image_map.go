package controllers

// An "image map" allows quick overriding of the images specified in VirtualMachine objects.
// That's handy during local development, for example, so that you can quickly swap images
// without having to change the compute image name in the control plane database. The image
// map is used by the compute.Tiltfile in the cloud repository to allow Tilt to replace the
// compute image; the Tiltfile commands regenerate the image mapping file with the tilt-generated
// image names whenever the compute images are rebuilt.
//
// An image map file consists of pairs of image names, specifying that when the VirtualMachine
// spec contains X, it is replaced with Y. If an image name is not present in the image map, it
// is used without changes.  For example, to replace v17 and v16 images with local versions,
// you could use this file:
//
// ```
// "vm-compute-node-v17:latest" = "vm-compute-node-v17:dev"
// "vm-compute-node-v16:latest" = "vm-compute-node-v16:dev"
// ```
//
// We use a toml file parser to parse the file, so toml syntax rules on comments and escaping
// apply.

import (
	"context"
	"os"

	"github.com/BurntSushi/toml"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ImageMap map[string]string

func EmptyImageMap() ImageMap {
	return map[string]string{}
}

func tryLoadImageMap(ctx context.Context, path string) ImageMap {
	if path == "" {
		return EmptyImageMap()
	}

	log := log.FromContext(ctx)

	newMap, err := loadImageMap(path)
	if err != nil {
		log.Error(err, "could not read image mapping file",
			"ImageMapPath", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		log.Error(err, "xcould not read image mapping file",
			"ImageMapPath", path)
		return EmptyImageMap()
	}

	log.Info("imagemap loaded", "map", newMap, "content", content)
	return newMap
}

func loadImageMap(path string) (ImageMap, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return EmptyImageMap(), err
	}
	var newMap map[string]string
	err = toml.Unmarshal(content, &newMap)
	if err != nil {
		return EmptyImageMap(), err
	}
	return newMap, nil
}

func (imageMap *ImageMap) mapImage(image string) string {
	mappedImage := (*imageMap)[image]
	if mappedImage != "" {
		return mappedImage
	} else {
		return image
	}
}
