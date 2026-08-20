package hypervisor

import "fmt"

// ValidateBootConfig validates boot and disk fields shared by hypervisor backends.
func ValidateBootConfig(cfg VMConfig) error {
	switch cfg.EffectiveBootMode() {
	case BootModeDirect:
		if cfg.Firmware != nil {
			return fmt.Errorf("direct boot cannot specify firmware")
		}
		if cfg.TPM != nil {
			return fmt.Errorf("direct boot cannot specify a TPM")
		}
	case BootModeUEFI:
		if cfg.Firmware == nil {
			return fmt.Errorf("UEFI boot requires firmware")
		}
		if cfg.Firmware.CodePath == "" || cfg.Firmware.VarsPath == "" {
			return fmt.Errorf("UEFI boot requires firmware code and variable storage paths")
		}
		if cfg.KernelPath != "" || cfg.InitrdPath != "" || cfg.KernelArgs != "" {
			return fmt.Errorf("UEFI boot cannot specify a direct kernel, initrd, or kernel arguments")
		}
		if cfg.TPM != nil && (cfg.TPM.SocketPath == "" || cfg.TPM.StateDir == "") {
			return fmt.Errorf("TPM requires socket and state directory paths")
		}
	default:
		return fmt.Errorf("unsupported boot mode %q", cfg.BootMode)
	}

	for i, disk := range cfg.Disks {
		switch disk.EffectiveFormat() {
		case DiskFormatRaw, DiskFormatQCOW2:
		default:
			return fmt.Errorf("disk %d has unsupported format %q", i, disk.Format)
		}
	}
	return nil
}

// ValidateDirectRawConfig preserves the Linux-only contract of backends that
// do not implement firmware boot or qcow2 disks.
func ValidateDirectRawConfig(backend string, cfg VMConfig) error {
	if err := ValidateBootConfig(cfg); err != nil {
		return err
	}
	if cfg.EffectiveBootMode() != BootModeDirect {
		return fmt.Errorf("%s does not support %s boot", backend, cfg.EffectiveBootMode())
	}
	for i, disk := range cfg.Disks {
		if disk.EffectiveFormat() != DiskFormatRaw {
			return fmt.Errorf("%s does not support disk %d format %q", backend, i, disk.EffectiveFormat())
		}
	}
	return nil
}
