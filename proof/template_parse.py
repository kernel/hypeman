#!/usr/bin/env python3
"""Parse the CloudFormation template.

Plain yaml.safe_load cannot read it: every !Ref/!Sub/!If is an unknown tag. This
loads the same document while accepting CloudFormation short-form intrinsics as
plain values and checks the sections the template must keep.
"""
import sys

import yaml


class CfnLoader(yaml.SafeLoader):
    pass


def short_form(loader, node):
    if isinstance(node, yaml.ScalarNode):
        return loader.construct_scalar(node)
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node)
    return loader.construct_mapping(node)


CfnLoader.add_multi_constructor("!", lambda loader, suffix, node: short_form(loader, node))

path = sys.argv[1] if len(sys.argv) > 1 else "deploy/aws/cloudformation/template.yaml"
with open(path) as handle:
    doc = yaml.load(handle, Loader=CfnLoader)

for section in ("AWSTemplateFormatVersion", "Parameters", "Resources", "Outputs"):
    if section not in doc:
        sys.exit(f"{path}: missing {section}")
user_data = doc["Resources"]["HypemanHost"]["Properties"]["UserData"]
assert "Fn::Base64" in user_data, "HypemanHost.UserData lost its Fn::Base64 wrapper"
print(f"{path}: parsed, {len(doc['Resources'])} resources")
