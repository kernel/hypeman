package cloudformation_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQuickstartParameters(t *testing.T) {
	template := loadTemplate(t)
	root := requireMapping(t, template)

	parameters := requireMapping(t, requireField(t, root, "Parameters"))
	assertDefault(t, parameters, "InstanceType", "c8i.2xlarge")
	assertDefault(t, parameters, "AllowedApiCidr", "127.0.0.1/32")
	assertDefault(t, parameters, "ApiPort", "8080")
	assertDefault(t, parameters, "EnableHttpIngress", "false")
	assertDefault(t, parameters, "EnableHttpsIngress", "false")
	assertDefault(t, parameters, "AllowedIngressCidr", "127.0.0.1/32")
	assertDefault(t, parameters, "EnableSSH", "false")
	assertDefault(t, parameters, "AllowedSshCidr", "127.0.0.1/32")
	assertDefault(t, parameters, "RootVolumeSize", "30")
	assertDefault(t, parameters, "DataVolumeSize", "100")
	assertDefault(t, parameters, "DataVolumeIops", "")
	assertDefault(t, parameters, "DataVolumeThroughput", "")
	assertDefault(t, parameters, "HypemanVersion", "latest")
	assertDefault(t, parameters, "HypemanCliVersion", "latest")

	instanceType := requireMapping(t, parameters["InstanceType"])
	assertContains(t, scalar(t, instanceType["AllowedPattern"]), "c8i|m8i|r8i")
	assertContains(t, scalar(t, instanceType["Description"]), "nested virtualization")

	apiCidr := requireMapping(t, parameters["AllowedApiCidr"])
	assertContains(t, scalar(t, apiCidr["Description"]), "current public IP /32")
	assertContains(t, scalar(t, apiCidr["Description"]), "avoid 0.0.0.0/0")

	ingressCidr := requireMapping(t, parameters["AllowedIngressCidr"])
	assertContains(t, scalar(t, ingressCidr["Description"]), "current public IP /32")
	assertContains(t, scalar(t, ingressCidr["Description"]), "avoid 0.0.0.0/0")

	metadata := requireMapping(t, requireField(t, root, "Metadata"))
	cfnInterface := requireMapping(t, requireField(t, metadata, "AWS::CloudFormation::Interface"))
	groups := requireSequence(t, requireField(t, cfnInterface, "ParameterGroups"))
	groupNames := make(map[string]bool)
	for _, group := range groups.Content {
		label := requireMapping(t, requireField(t, requireMapping(t, group), "Label"))
		groupNames[scalar(t, requireField(t, label, "default"))] = true
	}
	for _, name := range []string{"Network", "Instance", "Access", "Hypeman"} {
		if !groupNames[name] {
			t.Fatalf("missing CloudFormation parameter group %q", name)
		}
	}
}

func TestCloudFormationLaunchContract(t *testing.T) {
	template := loadTemplate(t)
	root := requireMapping(t, template)
	resources := requireMapping(t, requireField(t, root, "Resources"))

	securityGroup := requireMapping(t, requireField(t, resources, "HypemanSecurityGroup"))
	sgProperties := requireMapping(t, requireField(t, securityGroup, "Properties"))
	ingress := requireSequence(t, requireField(t, sgProperties, "SecurityGroupIngress"))
	if len(ingress.Content) != 4 {
		t.Fatalf("expected API ingress, HTTP ingress, HTTPS ingress, and SSH ingress, got %d entries", len(ingress.Content))
	}

	apiIngress := requireMapping(t, ingress.Content[0])
	assertRef(t, requireField(t, apiIngress, "FromPort"), "ApiPort")
	assertRef(t, requireField(t, apiIngress, "ToPort"), "ApiPort")
	assertRef(t, requireField(t, apiIngress, "CidrIp"), "AllowedApiCidr")

	assertConditionalIngress(t, ingress.Content[1], "UseHttpIngress", "80", "AllowedIngressCidr")
	assertConditionalIngress(t, ingress.Content[2], "UseHttpsIngress", "443", "AllowedIngressCidr")

	sshIngress := ingress.Content[3]
	if sshIngress.Tag != "!If" {
		t.Fatalf("expected SSH ingress to be conditional !If, got %s", sshIngress.Tag)
	}
	sshIf := requireSequence(t, sshIngress)
	if got := scalar(t, sshIf.Content[0]); got != "UseSSH" {
		t.Fatalf("expected SSH condition UseSSH, got %q", got)
	}
	sshRule := requireMapping(t, sshIf.Content[1])
	if got := scalar(t, requireField(t, sshRule, "FromPort")); got != "22" {
		t.Fatalf("expected SSH port 22, got %q", got)
	}
	assertRef(t, requireField(t, sshRule, "CidrIp"), "AllowedSshCidr")

	launchTemplate := requireMapping(t, requireField(t, resources, "NestedVirtualizationLaunchTemplate"))
	if got := scalar(t, requireField(t, launchTemplate, "Type")); got != "Custom::NestedVirtualizationLaunchTemplate" {
		t.Fatalf("NestedVirtualizationLaunchTemplate type = %q, want Custom::NestedVirtualizationLaunchTemplate", got)
	}

	launchTemplateFunction := requireMapping(t, requireField(t, resources, "NestedVirtualizationLaunchTemplateFunction"))
	code := requireMapping(t, requireField(t, requireMapping(t, requireField(t, launchTemplateFunction, "Properties")), "Code"))
	zipFile := scalar(t, requireField(t, code, "ZipFile"))
	assertContains(t, zipFile, `"Action": "CreateLaunchTemplate"`)
	assertContains(t, zipFile, `"LaunchTemplateData.CpuOptions.NestedVirtualization": "enabled"`)
	assertContains(t, zipFile, `"LaunchTemplateData.BlockDeviceMapping.1.Ebs.VolumeSize": props["RootVolumeSize"]`)
	assertContains(t, zipFile, `"LaunchTemplateData.BlockDeviceMapping.2.Ebs.VolumeSize": props["DataVolumeSize"]`)
	assertContains(t, zipFile, `"LaunchTemplateData.BlockDeviceMapping.2.Ebs.Iops"`)
	assertContains(t, zipFile, `"LaunchTemplateData.BlockDeviceMapping.2.Ebs.Throughput"`)

	launchTemplateProperties := requireMapping(t, requireField(t, launchTemplate, "Properties"))
	assertRef(t, requireField(t, launchTemplateProperties, "RootVolumeSize"), "RootVolumeSize")
	assertRef(t, requireField(t, launchTemplateProperties, "DataVolumeSize"), "DataVolumeSize")
	assertRef(t, requireField(t, launchTemplateProperties, "DataVolumeIops"), "DataVolumeIops")
	assertRef(t, requireField(t, launchTemplateProperties, "DataVolumeThroughput"), "DataVolumeThroughput")

	host := requireMapping(t, requireField(t, resources, "HypemanHost"))
	if got := scalar(t, requireField(t, host, "Type")); got != "AWS::EC2::Instance" {
		t.Fatalf("HypemanHost type = %q, want AWS::EC2::Instance", got)
	}
	hostProperties := requireMapping(t, requireField(t, host, "Properties"))
	hostLaunchTemplate := requireMapping(t, requireField(t, hostProperties, "LaunchTemplate"))
	assertGetAtt(t, requireField(t, hostLaunchTemplate, "LaunchTemplateId"), "NestedVirtualizationLaunchTemplate.LaunchTemplateId")
	assertGetAtt(t, requireField(t, hostLaunchTemplate, "Version"), "NestedVirtualizationLaunchTemplate.VersionNumber")

	userData := nodeText(requireField(t, hostProperties, "UserData"))
	assertContains(t, userData, "curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/install.sh | bash")
	assertContains(t, userData, `if [ -n "${DataVolumeThroughput}" ]; then`)
	assertContains(t, userData, `Environment="CAPACITY__DISK_IO=${DataVolumeThroughput}MB/s"`)
	assertContains(t, userData, "xfsprogs")
	assertContains(t, userData, "mkfs.xfs -f")
	assertContains(t, userData, "/var/lib/hypeman")
	assertContains(t, userData, `findmnt -n -o FSTYPE /var/lib/hypeman`)
	assertContains(t, userData, "/usr/local/bin/hypeman-create-token")
	assertContains(t, userData, "/opt/hypeman/deploy/validate.sh")
	assertContains(t, userData, "CONFIG_PATH=/etc/hypeman/config.yaml /opt/hypeman/bin/hypeman-token")
	assertContains(t, userData, "http://127.0.0.1:${ApiPort}/health")
	assertContains(t, userData, "GOMODCACHE=/root/go/pkg/mod")
}

func TestQuickstartOutputs(t *testing.T) {
	template := loadTemplate(t)
	root := requireMapping(t, template)
	outputs := requireMapping(t, requireField(t, root, "Outputs"))

	for _, name := range []string{
		"InstanceId",
		"PublicIp",
		"PrivateIp",
		"HypemanEndpoint",
		"SsmSessionCommand",
		"CreateTokenCommand",
	} {
		if _, ok := outputs[name]; !ok {
			t.Fatalf("missing output %q", name)
		}
	}

	assertContains(t, scalar(t, requireField(t, requireMapping(t, outputs["HypemanEndpoint"]), "Description")), "Hypeman API")
	assertContains(t, scalar(t, requireField(t, requireMapping(t, outputs["SsmSessionCommand"]), "Description")), "Session Manager")
	assertContains(t, scalar(t, requireField(t, requireMapping(t, outputs["CreateTokenCommand"]), "Value")), "hypeman-create-token")
}

func assertConditionalIngress(t *testing.T, node *yaml.Node, condition, port, cidrRef string) {
	t.Helper()

	if node.Tag != "!If" {
		t.Fatalf("expected ingress to be conditional !If, got %s", node.Tag)
	}
	parts := requireSequence(t, node)
	if got := scalar(t, parts.Content[0]); got != condition {
		t.Fatalf("expected condition %q, got %q", condition, got)
	}
	rule := requireMapping(t, parts.Content[1])
	if got := scalar(t, requireField(t, rule, "FromPort")); got != port {
		t.Fatalf("expected FromPort %s, got %q", port, got)
	}
	if got := scalar(t, requireField(t, rule, "ToPort")); got != port {
		t.Fatalf("expected ToPort %s, got %q", port, got)
	}
	assertRef(t, requireField(t, rule, "CidrIp"), cidrRef)
}

func loadTemplate(t *testing.T) *yaml.Node {
	t.Helper()

	raw, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) != 1 {
		t.Fatalf("expected one YAML document, got %d", len(doc.Content))
	}
	return doc.Content[0]
}

func assertDefault(t *testing.T, parameters map[string]*yaml.Node, name, want string) {
	t.Helper()

	parameter := requireMapping(t, requireField(t, parameters, name))
	if got := scalar(t, requireField(t, parameter, "Default")); got != want {
		t.Fatalf("parameter %s default = %q, want %q", name, got, want)
	}
}

func assertRef(t *testing.T, node *yaml.Node, want string) {
	t.Helper()

	if node.Kind != yaml.ScalarNode || node.Tag != "!Ref" || node.Value != want {
		t.Fatalf("expected !Ref %s, got kind=%v tag=%q value=%q", want, node.Kind, node.Tag, node.Value)
	}
}

func assertGetAtt(t *testing.T, node *yaml.Node, want string) {
	t.Helper()

	if node.Kind != yaml.ScalarNode || node.Tag != "!GetAtt" || node.Value != want {
		t.Fatalf("expected !GetAtt %s, got kind=%v tag=%q value=%q", want, node.Kind, node.Tag, node.Value)
	}
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()

	if !strings.Contains(got, want) {
		t.Fatalf("expected %q to contain %q", got, want)
	}
}

func requireField(t *testing.T, mapping map[string]*yaml.Node, key string) *yaml.Node {
	t.Helper()

	value, ok := mapping[key]
	if !ok {
		t.Fatalf("missing field %q", key)
	}
	return value
}

func requireMapping(t *testing.T, node *yaml.Node) map[string]*yaml.Node {
	t.Helper()

	if node.Kind != yaml.MappingNode {
		t.Fatalf("expected mapping node, got kind=%v tag=%q value=%q", node.Kind, node.Tag, node.Value)
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		result[node.Content[i].Value] = node.Content[i+1]
	}
	return result
}

func requireSequence(t *testing.T, node *yaml.Node) *yaml.Node {
	t.Helper()

	if node.Kind != yaml.SequenceNode {
		t.Fatalf("expected sequence node, got kind=%v tag=%q value=%q", node.Kind, node.Tag, node.Value)
	}
	return node
}

func scalar(t *testing.T, node *yaml.Node) string {
	t.Helper()

	if node.Kind != yaml.ScalarNode {
		t.Fatalf("expected scalar node, got kind=%v tag=%q", node.Kind, node.Tag)
	}
	return node.Value
}

func nodeText(node *yaml.Node) string {
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	var parts []string
	for _, child := range node.Content {
		parts = append(parts, nodeText(child))
	}
	return strings.Join(parts, "\n")
}
