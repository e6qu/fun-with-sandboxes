package dockerfile2llb

import (
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/client/llb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/dockerfile/instructions"
)

func dispatchRunDevices(c *instructions.RunCommand) ([]llb.RunOption, error) {
	var out []llb.RunOption
	for _, device := range instructions.GetDevices(c) {
		deviceOpts := []llb.CDIDeviceOption{
			llb.CDIDeviceName(device.Name),
		}
		if !device.Required {
			deviceOpts = append(deviceOpts, llb.CDIDeviceOptional)
		}
		out = append(out, llb.AddCDIDevice(deviceOpts...))
	}
	return out, nil
}
