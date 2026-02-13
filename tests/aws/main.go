package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"golang.org/x/crypto/ssh"
)

var startTime time.Time

func logf(format string, args ...any) {
	elapsed := time.Since(startTime)
	min := int(elapsed.Minutes())
	sec := int(elapsed.Seconds()) % 60
	prefix := fmt.Sprintf("[%02d:%02d]", min, sec)
	fmt.Printf(prefix+" "+format+"\n", args...)
}

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	region := flag.String("region", "us-east-1", "AWS region")
	instanceType := flag.String("instance-type", "c8id.48xlarge", "EC2 instance type")
	ami := flag.String("ami", "", "AMI ID (default: auto-detect latest Debian 12 amd64)")
	keyName := flag.String("key-name", "", "EC2 key pair name (required)")
	keyPath := flag.String("key-path", "", "Path to SSH private key (required)")
	profile := flag.String("profile", "", "AWS named profile")
	subnetID := flag.String("subnet-id", "", "Subnet ID (default: auto-detect from default VPC)")
	securityGroup := flag.String("security-group", "", "Existing security group ID")
	keep := flag.Bool("keep", false, "Don't terminate instance after test")
	skipSmoke := flag.Bool("skip-smoke", false, "Only verify nested virt + install, skip hypeman smoke test")
	skipBenchmark := flag.Bool("skip-benchmark", false, "Skip CoreMark benchmark")
	hypermanBranch := flag.String("hypeman-branch", "main", "Branch to install from")
	flag.Parse()

	if *keyName == "" || *keyPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --key-name and --key-path are required")
		flag.Usage()
		return 1
	}

	startTime = time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		logf("Interrupt received, cleaning up...")
		cancel()
	}()

	var cfgOpts []func(*awsconfig.LoadOptions) error
	cfgOpts = append(cfgOpts, awsconfig.WithRegion(*region))
	if *profile != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithSharedConfigProfile(*profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		logf("Failed to load AWS config: %v", err)
		return 1
	}
	svc := ec2.NewFromConfig(cfg)

	// Resources to clean up on exit.
	var instanceID string
	var createdSGID string
	defer func() {
		cleanCtx := context.Background()
		if instanceID != "" && !*keep {
			logf("Terminating instance %s...", instanceID)
			if _, err := svc.TerminateInstances(cleanCtx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{instanceID},
			}); err != nil {
				logf("Warning: failed to terminate instance: %v", err)
			}
		} else if instanceID != "" {
			logf("Keeping instance %s (--keep flag set)", instanceID)
		}
		if createdSGID != "" {
			if instanceID != "" && !*keep {
				logf("Waiting for instance to terminate before deleting security group...")
				w := ec2.NewInstanceTerminatedWaiter(svc)
				_ = w.Wait(cleanCtx, &ec2.DescribeInstancesInput{
					InstanceIds: []string{instanceID},
				}, 5*time.Minute)
			}
			logf("Deleting security group %s...", createdSGID)
			if _, err := svc.DeleteSecurityGroup(cleanCtx, &ec2.DeleteSecurityGroupInput{
				GroupId: aws.String(createdSGID),
			}); err != nil {
				logf("Warning: failed to delete security group: %v", err)
			}
		}
	}()

	timings := make(map[string]time.Duration)

	// --- Resolve AMI ---
	if *ami == "" {
		logf("Resolving AMI...")
		amiID, amiName, err := findDebianAMI(ctx, svc)
		if err != nil {
			logf("Failed to find Debian AMI: %v", err)
			return 1
		}
		*ami = amiID
		logf("Resolved AMI: %s (%s)", amiID, amiName)
	}

	// --- Resolve subnet ---
	if *subnetID == "" {
		logf("Resolving subnet from default VPC...")
		sid, err := findDefaultSubnet(ctx, svc)
		if err != nil {
			logf("Failed to find default subnet: %v", err)
			return 1
		}
		*subnetID = sid
		logf("Using subnet: %s", sid)
	}

	// --- Security group ---
	sgID := *securityGroup
	if sgID == "" {
		myIP, err := getMyIP()
		if err != nil {
			logf("Failed to detect public IP: %v", err)
			return 1
		}

		subnetOut, err := svc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			SubnetIds: []string{*subnetID},
		})
		if err != nil || len(subnetOut.Subnets) == 0 {
			logf("Failed to describe subnet: %v", err)
			return 1
		}
		vpcID := subnetOut.Subnets[0].VpcId

		logf("Creating security group (SSH from %s)...", myIP)
		sgName := fmt.Sprintf("hypeman-nested-virt-test-%s", time.Now().Format("20060102-150405"))
		sgOut, err := svc.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName:   aws.String(sgName),
			Description: aws.String("Temporary SG for hypeman nested virtualization test"),
			VpcId:       vpcID,
			TagSpecifications: []types.TagSpecification{{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String("hypeman-nested-virt-test")},
				},
			}},
		})
		if err != nil {
			logf("Failed to create security group: %v", err)
			return 1
		}
		sgID = *sgOut.GroupId
		createdSGID = sgID
		logf("Created security group: %s", sgID)

		_, err = svc.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []types.IpPermission{{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{{
					CidrIp:      aws.String(myIP + "/32"),
					Description: aws.String("SSH for nested virt test"),
				}},
			}},
		})
		if err != nil {
			logf("Failed to authorize SSH ingress: %v", err)
			return 1
		}
	}

	// --- UserData ---
	userdata := generateUserData(*hypermanBranch)
	userdataB64 := base64.StdEncoding.EncodeToString([]byte(userdata))

	// --- Launch instance ---
	logf("Launching %s with nested virtualization...", *instanceType)
	runInput := &ec2.RunInstancesInput{
		ImageId:      ami,
		InstanceType: types.InstanceType(*instanceType),
		KeyName:      keyName,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		UserData:     aws.String(userdataB64),
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			SubnetId:                 subnetID,
			AssociatePublicIpAddress: aws.Bool(true),
			Groups:                   []string{sgID},
		}},
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeInstance,
			Tags: []types.Tag{
				{Key: aws.String("Name"), Value: aws.String("hypeman-nested-virt-test")},
			},
		}},
	}
	// Bare metal instances don't support CpuOptions.
	if !strings.Contains(*instanceType, "metal") {
		runInput.CpuOptions = &types.CpuOptionsRequest{
			NestedVirtualization: "enabled",
		}
	}
	runOut, err := svc.RunInstances(ctx, runInput)
	if err != nil {
		logf("Failed to launch instance: %v", err)
		return 1
	}
	instanceID = *runOut.Instances[0].InstanceId
	logf("Instance %s launched", instanceID)

	// --- Wait for running ---
	logf("Waiting for instance to be running...")
	runWaiter := ec2.NewInstanceRunningWaiter(svc)
	if err := runWaiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 15*time.Minute); err != nil {
		logf("Instance failed to reach running state: %v", err)
		return 1
	}
	timings["running"] = time.Since(startTime)
	logf("Instance running (%s)", timings["running"].Round(time.Second))

	// Get public IP.
	descOut, err := svc.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil || len(descOut.Reservations) == 0 || len(descOut.Reservations[0].Instances) == 0 {
		logf("Failed to describe instance: %v", err)
		return 1
	}
	publicIP := aws.ToString(descOut.Reservations[0].Instances[0].PublicIpAddress)
	if publicIP == "" {
		logf("Instance has no public IP - ensure subnet has auto-assign public IP enabled")
		return 1
	}
	logf("Public IP: %s", publicIP)

	// --- SSH ---
	sshClient, err := waitForSSH(ctx, publicIP, *keyPath)
	if err != nil {
		logf("SSH failed: %v", err)
		return 1
	}
	defer sshClient.Close()
	timings["ssh"] = time.Since(startTime)
	logf("SSH ready (%s)", timings["ssh"].Round(time.Second))

	// --- Verify KVM ---
	out, err := sshRun(sshClient, "ls -la /dev/kvm")
	if err != nil {
		logf("/dev/kvm not found: %v\n%s", err, out)
		return 1
	}
	logf("/dev/kvm verified - nested virtualization works!")

	out, err = sshRun(sshClient, "dmesg | grep -i kvm | head -5")
	if err == nil && strings.TrimSpace(out) != "" {
		logf("KVM dmesg:\n%s", strings.TrimSpace(out))
	}

	// --- Wait for userdata completion ---
	logf("Waiting for userdata to complete (hypeman install)...")
	if err := waitForUserdata(ctx, sshClient); err != nil {
		logOutput, _ := sshRun(sshClient, "cat /var/log/userdata.log 2>/dev/null || echo 'no log'")
		logf("Userdata log:\n%s", logOutput)
		logf("Userdata did not complete: %v", err)
		return 1
	}
	timings["installed"] = time.Since(startTime)
	logf("Userdata complete - hypeman installed (%s)", timings["installed"].Round(time.Second))

	out, err = sshRun(sshClient, "sudo systemctl status hypeman --no-pager")
	if err != nil {
		logf("Warning: hypeman service status: %v\n%s", err, out)
	} else {
		logf("Hypeman service is active")
	}

	// --- Smoke test ---
	if !*skipSmoke {
		logf("Starting smoke test...")

		// Pull image.
		out, err = sshRun(sshClient, "sudo hypeman pull alpine:latest")
		if err != nil {
			logf("Failed to pull alpine: %v\n%s", err, out)
			return 1
		}
		logf("Smoke test: pull initiated\n%s", strings.TrimSpace(out))

		// Wait for image to be ready (pull is async).
		logf("Waiting for image to become ready...")
		if err := waitForImageReady(ctx, sshClient); err != nil {
			logf("Image did not become ready: %v", err)
			return 1
		}
		logf("Image ready")

		// --- Cloud Hypervisor (default) ---
		chStart := time.Now()
		out, err = sshRun(sshClient, "sudo hypeman run --name alpine-ch alpine:latest")
		if err != nil {
			logf("Failed to run alpine (cloud-hypervisor): %v\n%s", err, out)
			return 1
		}
		timings["ch_run"] = time.Since(chStart)
		logf("Smoke test: cloud-hypervisor instance launched (%s)\n%s",
			timings["ch_run"].Round(time.Millisecond), strings.TrimSpace(out))

		// Verify CH instance is running.
		time.Sleep(2 * time.Second)
		out, err = sshRun(sshClient, "sudo hypeman ps")
		if err != nil {
			logf("Failed to list instances: %v\n%s", err, out)
			return 1
		}
		if !strings.Contains(out, "alpine-ch") {
			logf("Cloud-hypervisor instance not found in ps:\n%s", strings.TrimSpace(out))
			return 1
		}
		logf("Smoke test: cloud-hypervisor instance verified running")

		// Stop CH before QEMU — both use the same rootfs.ext4 and the
		// filesystem lock prevents concurrent access.
		logf("Stopping cloud-hypervisor instance before QEMU test...")
		sshRun(sshClient, "sudo hypeman stop alpine-ch")
		time.Sleep(1 * time.Second)
		sshRun(sshClient, "sudo hypeman rm alpine-ch")
		time.Sleep(1 * time.Second)

		// --- QEMU ---
		qemuStart := time.Now()
		out, err = sshRun(sshClient, "sudo hypeman run --name alpine-qemu --hypervisor qemu alpine:latest")
		if err != nil {
			apiLogs, _ := sshRun(sshClient, "sudo journalctl -u hypeman --no-pager -n 50")
			logf("Failed to run alpine (qemu): %v\n%s\nAPI logs:\n%s", err, out, apiLogs)
			return 1
		}
		timings["qemu_run"] = time.Since(qemuStart)
		logf("Smoke test: qemu instance launched (%s)\n%s",
			timings["qemu_run"].Round(time.Millisecond), strings.TrimSpace(out))

		// Verify QEMU instance is running.
		time.Sleep(2 * time.Second)
		out, err = sshRun(sshClient, "sudo hypeman ps")
		if err != nil {
			logf("Failed to list instances: %v\n%s", err, out)
			return 1
		}
		if !strings.Contains(out, "alpine-qemu") {
			logf("QEMU instance not found in ps output:\n%s", strings.TrimSpace(out))
			return 1
		}
		logf("Smoke test: qemu instance verified running")

		// Clean up smoke test instances.
		sshRun(sshClient, "sudo hypeman stop alpine-qemu")
		sshRun(sshClient, "sudo hypeman rm alpine-qemu")

		timings["smoke"] = time.Since(startTime)
		logf("All smoke tests passed! Both cloud-hypervisor and QEMU work with nested virtualization.")
	} else {
		logf("Skipping smoke test (--skip-smoke)")
	}

	// --- CoreMark benchmark ---
	var hostScore, vmScore float64
	if !*skipBenchmark {
		logf("Starting CoreMark benchmark...")
		var err error
		hostScore, vmScore, err = runCoreMark(ctx, sshClient)
		if err != nil {
			// VM benchmark may fail in L2 nested virt where VMs crash
			// inside a VM. Only treat as a warning for non-bare-metal
			// instances (which use nested/L2 virtualization); on bare
			// metal the VM benchmark is expected to work.
			isBareMetal := strings.Contains(*instanceType, "metal")
			if hostScore > 0 && !isBareMetal {
				logf("CoreMark VM benchmark failed (L2 nested virt, host score available): %v", err)
			} else {
				logf("CoreMark benchmark failed: %v", err)
				return 1
			}
		}
		timings["benchmark"] = time.Since(startTime)
	} else {
		logf("Skipping CoreMark benchmark (--skip-benchmark)")
	}

	// --- Timing summary ---
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════")
	fmt.Printf("  Results: %s\n", *instanceType)
	fmt.Println("═══════════════════════════════════════════")
	fmt.Println()
	fmt.Println("Timing:")
	fmt.Printf("  Launch -> Running:      %s\n", timings["running"].Round(time.Second))
	fmt.Printf("  Launch -> SSH Ready:    %s\n", timings["ssh"].Round(time.Second))
	fmt.Printf("  Launch -> Installed:    %s\n", timings["installed"].Round(time.Second))
	if t, ok := timings["ch_run"]; ok {
		fmt.Printf("  Cloud Hypervisor run:   %s\n", t.Round(time.Millisecond))
	}
	if t, ok := timings["qemu_run"]; ok {
		fmt.Printf("  QEMU run:               %s\n", t.Round(time.Millisecond))
	}
	if t, ok := timings["smoke"]; ok {
		fmt.Printf("  Launch -> Smoke Test:   %s\n", t.Round(time.Second))
	}
	if !*skipBenchmark && hostScore > 0 {
		fmt.Println()
		fmt.Println("CoreMark:")
		fmt.Printf("  Host score:             %.2f iterations/sec\n", hostScore)
		if vmScore > 0 {
			fmt.Printf("  VM score:               %.2f iterations/sec\n", vmScore)
			overhead := (1.0 - vmScore/hostScore) * 100
			fmt.Printf("  Virtualization overhead: %.1f%%\n", overhead)
		} else {
			fmt.Printf("  VM score:               N/A (L2 VMs not supported)\n")
		}
		if t, ok := timings["benchmark"]; ok {
			fmt.Printf("  Launch -> Benchmark:    %s\n", t.Round(time.Second))
		}
	}
	fmt.Println()

	return 0
}

// runCoreMark compiles CoreMark on the host, runs it, then creates a VM
// with all available CPUs/RAM and runs CoreMark inside it for comparison.
func runCoreMark(ctx context.Context, client *ssh.Client) (hostScore, vmScore float64, err error) {
	// Clone and compile CoreMark as a static binary.
	logf("CoreMark: cloning repository...")
	out, err := sshRun(client, "git clone --depth 1 https://github.com/eembc/coremark.git /tmp/coremark 2>&1")
	if err != nil {
		return 0, 0, fmt.Errorf("clone: %v\n%s", err, out)
	}

	logf("CoreMark: compiling (static binary)...")
	out, err = sshRun(client, "cd /tmp/coremark && make XCFLAGS='-static' 2>&1")
	if err != nil {
		return 0, 0, fmt.Errorf("compile: %v\n%s", err, out)
	}

	// Verify binary exists.
	out, err = sshRun(client, "ls -la /tmp/coremark/coremark.exe")
	if err != nil {
		return 0, 0, fmt.Errorf("binary not found: %v\n%s", err, out)
	}

	// Run on host.
	logf("CoreMark: running on host (this takes ~15s)...")
	hostOut, err := sshRun(client, "cd /tmp/coremark && ./coremark.exe")
	if err != nil {
		return 0, 0, fmt.Errorf("host run: %v\n%s", err, hostOut)
	}
	hostScore = parseCoreMarkScore(hostOut)
	if hostScore == 0 {
		logf("CoreMark host output:\n%s", hostOut)
		return 0, 0, fmt.Errorf("failed to parse host CoreMark score")
	}
	logf("CoreMark host: %.2f iterations/sec", hostScore)

	// Detect host CPUs for reporting.
	nprocOut, _ := sshRun(client, "nproc")
	hostCPUs, _ := strconv.Atoi(strings.TrimSpace(nprocOut))
	if hostCPUs == 0 {
		hostCPUs = 2
	}

	// Pull alpine if not already available.
	sshRun(client, "sudo hypeman pull alpine:latest 2>/dev/null")
	waitForImageReady(ctx, client)

	// Create VM with QEMU hypervisor. CoreMark is single-threaded so vCPU count
	// doesn't affect the score — only per-core performance matters.
	// Note: Cloud Hypervisor VMs enter Shutdown state immediately in L2 nested
	// virt, so we use QEMU which handles nested virt more reliably.
	logf("CoreMark: creating VM (QEMU, 2 vCPUs, host has %d CPUs)...", hostCPUs)
	vmCmd := "sudo hypeman run --name bench-vm --hypervisor qemu alpine:latest"
	out, err = sshRun(client, vmCmd)
	if err != nil {
		return hostScore, 0, fmt.Errorf("create VM: %v\n%s", err, out)
	}
	logf("CoreMark: VM created, waiting for guest agent...")

	// Wait for guest agent to be ready.
	if err := waitForGuestAgent(ctx, client, "bench-vm"); err != nil {
		logs, _ := sshRun(client, "sudo journalctl -u hypeman --no-pager -n 30")
		return hostScore, 0, fmt.Errorf("guest agent not ready: %v\nLogs:\n%s", err, logs)
	}
	logf("CoreMark: guest agent ready")

	// Copy static CoreMark binary into VM.
	logf("CoreMark: copying binary to VM...")
	out, err = sshRun(client, "sudo hypeman cp /tmp/coremark/coremark.exe bench-vm:/tmp/coremark")
	if err != nil {
		return hostScore, 0, fmt.Errorf("cp to VM: %v\n%s", err, out)
	}
	out, err = sshRun(client, "sudo hypeman exec bench-vm -- chmod +x /tmp/coremark")
	if err != nil {
		return hostScore, 0, fmt.Errorf("chmod: %v\n%s", err, out)
	}

	// Run CoreMark inside VM.
	logf("CoreMark: running in VM (this takes ~15s)...")
	vmOut, err := sshRun(client, "sudo hypeman exec bench-vm -- /tmp/coremark")
	if err != nil {
		return hostScore, 0, fmt.Errorf("VM run: %v\n%s", err, vmOut)
	}
	vmScore = parseCoreMarkScore(vmOut)
	if vmScore == 0 {
		logf("CoreMark VM output:\n%s", vmOut)
		return hostScore, 0, fmt.Errorf("failed to parse VM CoreMark score")
	}
	logf("CoreMark VM: %.2f iterations/sec", vmScore)

	overhead := (1.0 - vmScore/hostScore) * 100
	logf("CoreMark virtualization overhead: %.1f%%", overhead)

	// Clean up benchmark VM.
	sshRun(client, "sudo hypeman stop bench-vm")
	sshRun(client, "sudo hypeman rm bench-vm")

	return hostScore, vmScore, nil
}

// parseCoreMarkScore extracts the iterations/sec from CoreMark output.
// Looks for "Iterations/Sec   : NNNN.NNNN" or "CoreMark 1.0 : NNNN.NNNN".
func parseCoreMarkScore(output string) float64 {
	for _, line := range strings.Split(output, "\n") {
		// Primary: "Iterations/Sec   : 30000.000000"
		if strings.Contains(line, "Iterations/Sec") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				score, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				if err == nil {
					return score
				}
			}
		}
		// Fallback: "CoreMark 1.0 : 30000.000000 / GCC..."
		if strings.Contains(line, "CoreMark 1.0") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				scorePart := strings.TrimSpace(parts[1])
				slashIdx := strings.Index(scorePart, "/")
				if slashIdx > 0 {
					scorePart = strings.TrimSpace(scorePart[:slashIdx])
				}
				score, err := strconv.ParseFloat(scorePart, 64)
				if err == nil {
					return score
				}
			}
		}
	}
	return 0
}

// waitForGuestAgent polls until `hypeman exec` succeeds on the given instance.
func waitForGuestAgent(ctx context.Context, client *ssh.Client, instanceName string) error {
	deadline := time.Now().Add(5 * time.Minute)
	cmd := fmt.Sprintf("sudo hypeman exec %s -- echo ready", instanceName)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("guest agent timeout after 5 minutes")
			}
			out, err := sshRun(client, cmd)
			if err == nil && strings.Contains(out, "ready") {
				return nil
			}
		}
	}
}

// findDebianAMI finds the latest Debian 12 amd64 AMI.
func findDebianAMI(ctx context.Context, svc *ec2.Client) (id, name string, err error) {
	out, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"136693071363"}, // Debian official
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"debian-12-amd64-*"}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("DescribeImages: %w", err)
	}
	if len(out.Images) == 0 {
		return "", "", fmt.Errorf("no Debian 12 amd64 AMIs found")
	}
	images := out.Images
	sort.Slice(images, func(i, j int) bool {
		return aws.ToString(images[i].CreationDate) > aws.ToString(images[j].CreationDate)
	})
	return aws.ToString(images[0].ImageId), aws.ToString(images[0].Name), nil
}

// findDefaultSubnet returns the first default subnet in the default VPC.
func findDefaultSubnet(ctx context.Context, svc *ec2.Client) (string, error) {
	vpcs, err := svc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{
			{Name: aws.String("is-default"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeVpcs: %w", err)
	}
	if len(vpcs.Vpcs) == 0 {
		return "", fmt.Errorf("no default VPC found")
	}
	subnets, err := svc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{aws.ToString(vpcs.Vpcs[0].VpcId)}},
			{Name: aws.String("default-for-az"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeSubnets: %w", err)
	}
	if len(subnets.Subnets) == 0 {
		return "", fmt.Errorf("no default subnets found in default VPC")
	}
	return aws.ToString(subnets.Subnets[0].SubnetId), nil
}

// getMyIP returns the caller's public IPv4 address.
func getMyIP() (string, error) {
	resp, err := http.Get("https://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// generateUserData returns the cloud-init script with the given branch substituted.
func generateUserData(branch string) string {
	return fmt.Sprintf(`#!/bin/bash
set -ex
exec > >(tee /var/log/userdata.log) 2>&1
echo "STARTED $(date +%%s)"

# Ensure HOME is set (userdata runs as root but HOME may be unset)
export HOME=/root

# Wait for network
until curl -sf https://checkip.amazonaws.com > /dev/null 2>&1; do sleep 1; done

# Install build dependencies, QEMU, and gcc (for CoreMark benchmark)
apt-get update -qq
apt-get install -y -qq git make curl qemu-system-x86 build-essential

# Install Go (needed for BRANCH mode)
curl -fsSL https://go.dev/dl/go1.25.4.linux-amd64.tar.gz | tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
export GOPATH=/root/go

# Install hypeman
export BRANCH="%s"
curl -fsSL "https://raw.githubusercontent.com/kernel/hypeman/%s/scripts/install.sh" | bash

# Verify KVM is available
ls -la /dev/kvm
echo "KVM_AVAILABLE"

echo "COMPLETED $(date +%%s)"
echo "OK" > /tmp/userdata-complete
`, branch, branch)
}

// waitForSSH polls until an SSH connection succeeds, then returns the client.
func waitForSSH(ctx context.Context, host, keyPath string) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH key: %w", err)
	}

	sshCfg := &ssh.ClientConfig{
		User:            "admin", // Debian AMI default user
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(host, "22")
	deadline := time.Now().Add(15 * time.Minute) // Bare metal can take >5 min

	// Try immediately, then poll every 30s.
	if client, err := ssh.Dial("tcp", addr, sshCfg); err == nil {
		return client, nil
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("SSH timeout after 15 minutes")
			}
			client, err := ssh.Dial("tcp", addr, sshCfg)
			if err != nil {
				logf("SSH not ready: %v", err)
				continue
			}
			return client, nil
		}
	}
}

// sshRun executes a command on the remote host and returns combined output.
func sshRun(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("creating SSH session: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// waitForImageReady polls `hypeman image list` until alpine shows as "ready".
func waitForImageReady(ctx context.Context, client *ssh.Client) error {
	deadline := time.Now().Add(3 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("image readiness timeout after 3 minutes")
			}
			out, err := sshRun(client, "sudo hypeman image list -q")
			if err != nil {
				continue
			}
			// If quiet mode lists the image name, check full output for status.
			if strings.Contains(out, "alpine") {
				full, err := sshRun(client, "sudo hypeman image list")
				if err != nil {
					continue
				}
				if strings.Contains(full, "ready") {
					return nil
				}
				logf("Image status: %s", strings.TrimSpace(full))
			}
		}
	}
}

// waitForUserdata polls for the /tmp/userdata-complete marker file.
func waitForUserdata(ctx context.Context, client *ssh.Client) error {
	deadline := time.Now().Add(10 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("userdata timeout after 10 minutes")
			}
			out, err := sshRun(client, "cat /tmp/userdata-complete 2>/dev/null")
			if err == nil && strings.TrimSpace(out) == "OK" {
				return nil
			}
		}
	}
}
