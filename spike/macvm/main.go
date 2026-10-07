//go:build darwin && arm64

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

func init() { runtime.LockOSThread() }

type config struct {
	HardwareModel     []byte `json:"hardware_model"`
	MachineIdentifier []byte `json:"machine_identifier"`
	MAC               string `json:"mac"`
	CPUs              uint   `json:"cpus"`
	Memory            uint64 `json:"memory"`
}

func main() {
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func readConfig(dir string) (config, error) {
	var c config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}
func writeConfig(dir string, c config) error {
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0600)
}
func execute() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("commands: fetch-ipsw, install, run, ctl, validate, clone")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := fs.String("dir", "", "VM directory")
	out := fs.String("out", "", "output IPSW")
	ipsw := fs.String("ipsw", "", "IPSW path")
	diskGB := fs.Int64("disk-gb", 64, "disk GiB")
	cpus := fs.Uint("cpus", 0, "CPU count")
	mem := fs.Uint64("mem-gb", 0, "RAM GiB")
	gui := fs.Bool("gui", false, "open GUI")
	profile := fs.String("profile", "suspendable", "suspendable or full")
	devices := fs.String("devices", "", "exact device list (validate only)")
	restore := fs.String("restore", "", "saved state")
	paused := fs.Bool("stay-paused", false, "don't resume restored VM")
	src := fs.String("src", "", "clone source")
	dst := fs.String("dst", "", "clone destination")
	rekey := fs.Bool("rekey", false, "new identifier")
	newMAC := fs.Bool("new-mac", false, "new MAC")
	state := fs.String("with-state", "", "clone saved state")
	if e := fs.Parse(os.Args[2:]); e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch cmd {
	case "fetch-ipsw":
		if *out == "" {
			return fmt.Errorf("--out required")
		}
		r, e := vz.FetchLatestSupportedMacOSRestoreImage(ctx, *out)
		if e != nil {
			return e
		}
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-r.Finished():
				if e := r.Err(); e != nil {
					return e
				}
				image, e := vz.LoadMacOSRestoreImageFromPath(*out)
				if e != nil {
					return e
				}
				fmt.Printf("version=%s build=%s bytes=%d\n", image.OperatingSystemVersion(), image.BuildVersion(), r.Current())
				return nil
			case <-tick.C:
				fmt.Printf("download %.1f%% bytes=%d\n", 100*r.FractionCompleted(), r.Current())
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	case "install":
		if *dir == "" || *ipsw == "" {
			return fmt.Errorf("--dir and --ipsw required")
		}
		if *diskGB < 32 {
			return fmt.Errorf("disk must be at least 32 GiB")
		}
		if _, e := os.Stat(*ipsw); e != nil {
			return e
		}
		if _, e := os.Stat(filepath.Join(*dir, "config.json")); !os.IsNotExist(e) {
			return fmt.Errorf("refusing to overwrite existing VM")
		}
		if e := os.MkdirAll(*dir, 0700); e != nil {
			return e
		}
		image, e := vz.LoadMacOSRestoreImageFromPath(*ipsw)
		if e != nil {
			return e
		}
		req := image.MostFeaturefulSupportedConfiguration()
		model := req.HardwareModel()
		if !model.Supported() {
			return fmt.Errorf("unsupported hardware model")
		}
		id, e := vz.NewMacMachineIdentifier()
		if e != nil {
			return e
		}
		mac, e := vz.NewRandomLocallyAdministeredMACAddress()
		if e != nil {
			return e
		}
		ncpu := *cpus
		if ncpu == 0 {
			ncpu = 4
		}
		memory := *mem
		if memory == 0 {
			memory = 8
		}
		c := config{model.DataRepresentation(), id.DataRepresentation(), mac.String(), ncpu, memory << 30}
		if uint64(c.CPUs) < req.MinimumSupportedCPUCount() || c.Memory < req.MinimumSupportedMemorySize() {
			return fmt.Errorf("resources below IPSW requirements")
		}
		if _, e := vz.NewMacAuxiliaryStorage(filepath.Join(*dir, "aux.img"), vz.WithCreatingMacAuxiliaryStorage(model)); e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(*dir, "disk.img"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		e = f.Truncate(*diskGB << 30)
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if e := writeConfig(*dir, c); e != nil {
			return e
		}
		vc, e := build(*dir, c, "mac-graphics,mac-input,vsock,nat-net,blk")
		if e != nil {
			return e
		}
		if ok, err := vc.Validate(); !ok || err != nil {
			return fmt.Errorf("invalid installer VM configuration: %v", err)
		}
		vm, e := vz.NewVirtualMachine(vc)
		if e != nil {
			return e
		}
		installer, e := vz.NewMacOSInstaller(vm, *ipsw)
		if e != nil {
			return e
		}
		done := make(chan error, 1)
		go func() { done <- installer.Install(ctx) }()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		start := time.Now()
		for {
			select {
			case e := <-done:
				if e != nil {
					return e
				}
				fmt.Printf("install elapsed=%s version=%s build=%s\n", time.Since(start), image.OperatingSystemVersion(), image.BuildVersion())
				return nil
			case <-tick.C:
				fmt.Printf("install %.1f%%\n", 100*installer.FractionCompleted())
			}
		}
	case "clone":
		if *src == "" || *dst == "" {
			return fmt.Errorf("--src and --dst required")
		}
		c, e := readConfig(*src)
		if e != nil {
			return e
		}
		if e := os.Mkdir(*dst, 0700); e != nil {
			return e
		}
		for _, name := range []string{"disk.img", "aux.img"} {
			if e := unix.Clonefile(filepath.Join(*src, name), filepath.Join(*dst, name), 0); e != nil {
				return e
			}
		}
		if *rekey {
			id, e := vz.NewMacMachineIdentifier()
			if e != nil {
				return e
			}
			c.MachineIdentifier = id.DataRepresentation()
		}
		if *newMAC {
			mac, e := vz.NewRandomLocallyAdministeredMACAddress()
			if e != nil {
				return e
			}
			c.MAC = mac.String()
		}
		if *state != "" {
			if e := unix.Clonefile(*state, filepath.Join(*dst, "machine-state.vzm"), 0); e != nil {
				return e
			}
		}
		return writeConfig(*dst, c)
	case "ctl":
		if *dir == "" || len(fs.Args()) == 0 {
			return fmt.Errorf("--dir and action required")
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(*dir, "ctl.sock"))
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Minute}
		body := strings.Join(fs.Args()[1:], " ")
		r, e := client.Post("http://vm/"+fs.Args()[0], "text/plain", strings.NewReader(body))
		if e != nil {
			return e
		}
		defer r.Body.Close()
		var result map[string]any
		if e := json.NewDecoder(r.Body).Decode(&result); e != nil {
			return e
		}
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		if r.StatusCode != 200 {
			return fmt.Errorf("control failed: %s", r.Status)
		}
		return nil
	case "validate", "run":
		if *dir == "" {
			return fmt.Errorf("--dir required")
		}
		c, e := readConfig(*dir)
		if e != nil {
			return e
		}
		if *cpus != 0 {
			c.CPUs = *cpus
		}
		if *mem != 0 {
			c.Memory = *mem << 30
		}
		list := "mac-graphics,mac-input,vsock,nat-net,blk"
		if *profile == "full" {
			list = "mac-graphics,usb-input,entropy,vsock,nat-net,blk"
		} else if *profile != "suspendable" {
			return fmt.Errorf("unknown profile")
		}
		if cmd == "validate" && *devices != "" {
			list = *devices
		}
		vc, e := build(*dir, c, list)
		if e != nil {
			return e
		}
		if cmd == "validate" {
			ok, ve := vc.Validate()
			save, se := vc.ValidateSaveRestoreSupport()
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"devices": list, "valid": ok, "validation_error": errText(ve), "save_restore": save, "save_restore_error": errText(se)})
		}
		return run(ctx, *dir, vc, *gui, *restore, *paused)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}
func errText(e error) string {
	if e != nil {
		return e.Error()
	}
	return ""
}
func build(dir string, c config, list string) (*vz.VirtualMachineConfiguration, error) {
	boot, e := vz.NewMacOSBootLoader()
	if e != nil {
		return nil, e
	}
	vc, e := vz.NewVirtualMachineConfiguration(boot, c.CPUs, c.Memory)
	if e != nil {
		return nil, e
	}
	model, e := vz.NewMacHardwareModelWithData(c.HardwareModel)
	if e != nil {
		return nil, e
	}
	id, e := vz.NewMacMachineIdentifierWithData(c.MachineIdentifier)
	if e != nil {
		return nil, e
	}
	aux, e := vz.NewMacAuxiliaryStorage(filepath.Join(dir, "aux.img"))
	if e != nil {
		return nil, e
	}
	platform, e := vz.NewMacPlatformConfiguration(vz.WithMacHardwareModel(model), vz.WithMacMachineIdentifier(id), vz.WithMacAuxiliaryStorage(aux))
	if e != nil {
		return nil, e
	}
	vc.SetPlatformVirtualMachineConfiguration(platform)
	for _, device := range strings.Split(list, ",") {
		switch device {
		case "mac-graphics":
			g, e := vz.NewMacGraphicsDeviceConfiguration()
			if e != nil {
				return nil, e
			}
			d, e := vz.NewMacGraphicsDisplayConfiguration(1280, 800, 80)
			if e != nil {
				return nil, e
			}
			g.SetDisplays(d)
			vc.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{g})
		case "mac-input":
			k, e := vz.NewMacKeyboardConfiguration()
			if e != nil {
				return nil, e
			}
			p, e := vz.NewMacTrackpadConfiguration()
			if e != nil {
				return nil, e
			}
			vc.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{k})
			vc.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{p})
		case "usb-input":
			k, e := vz.NewUSBKeyboardConfiguration()
			if e != nil {
				return nil, e
			}
			p, e := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
			if e != nil {
				return nil, e
			}
			vc.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{k})
			vc.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{p})
		case "blk":
			a, e := vz.NewDiskImageStorageDeviceAttachment(filepath.Join(dir, "disk.img"), false)
			if e != nil {
				return nil, e
			}
			d, e := vz.NewVirtioBlockDeviceConfiguration(a)
			if e != nil {
				return nil, e
			}
			vc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{d})
		case "nat-net":
			a, e := vz.NewNATNetworkDeviceAttachment()
			if e != nil {
				return nil, e
			}
			n, e := vz.NewVirtioNetworkDeviceConfiguration(a)
			if e != nil {
				return nil, e
			}
			hw, e := net.ParseMAC(c.MAC)
			if e != nil {
				return nil, e
			}
			m, e := vz.NewMACAddress(hw)
			if e != nil {
				return nil, e
			}
			n.SetMACAddress(m)
			vc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{n})
		case "vsock":
			s, e := vz.NewVirtioSocketDeviceConfiguration()
			if e != nil {
				return nil, e
			}
			vc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{s})
		case "entropy":
			d, e := vz.NewVirtioEntropyDeviceConfiguration()
			if e != nil {
				return nil, e
			}
			vc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{d})
		case "balloon":
			d, e := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
			if e != nil {
				return nil, e
			}
			vc.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{d})
		default:
			return nil, fmt.Errorf("device %q not implemented", device)
		}
	}
	return vc, nil
}
func run(ctx context.Context, dir string, vc *vz.VirtualMachineConfiguration, gui bool, restore string, paused bool) error {
	if ok, e := vc.Validate(); !ok || e != nil {
		return fmt.Errorf("invalid VM: %v", e)
	}
	vm, e := vz.NewVirtualMachine(vc)
	if e != nil {
		return e
	}
	log, e := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		enc := json.NewEncoder(log)
		for s := range vm.StateChangedNotify() {
			enc.Encode(map[string]any{"timestamp_ns": time.Now().UnixNano(), "state": s.String()})
			fmt.Println("state", s)
			if s == vz.VirtualMachineStateStopped || s == vz.VirtualMachineStateError {
				return
			}
		}
	}()
	socket := filepath.Join(dir, "ctl.sock")
	ln, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var e error
		switch r.URL.Path {
		case "/status":
		case "/pause":
			e = vm.Pause()
		case "/resume":
			e = vm.Resume()
		case "/stop":
			e = vm.Stop()
		case "/request-stop":
			_, e = vm.RequestStop()
		case "/save":
			var b []byte
			b, e = io.ReadAll(r.Body)
			if e == nil {
				e = vm.SaveMachineStateToPath(string(b))
			}
		default:
			e = fmt.Errorf("unknown action")
		}
		if e != nil {
			w.WriteHeader(500)
		}
		json.NewEncoder(w).Encode(map[string]any{"state": vm.State().String(), "error": errText(e)})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(ln)
	defer server.Close()
	if restore != "" {
		e = vm.RestoreMachineStateFromURL(restore)
		if e == nil && !paused {
			e = vm.Resume()
		}
	} else {
		e = vm.Start()
	}
	if e != nil {
		return e
	}
	go func() {
		select {
		case <-ctx.Done():
			vm.Stop()
		case <-done:
		}
	}()
	if gui {
		return vm.StartGraphicApplication(1280, 800, vz.WithWindowTitle("Hypeman macOS spike"))
	}
	select {
	case <-done:
		if vm.State() == vz.VirtualMachineStateError {
			return fmt.Errorf("VM entered error state")
		}
		return nil
	case <-ctx.Done():
		<-done
		return ctx.Err()
	}
}
