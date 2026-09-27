resource "aws_vpc" "⟦org:romashka⟧" {
  cidr_block           = "⟦cidr4:10.120.0.0/16⟧"
  enable_dns_hostnames = true
  tags = { Owner = "⟦email:infra@romashka.example⟧" }
}

resource "aws_subnet" "private_a" {
  vpc_id            = aws_vpc.⟦org:romashka⟧.id
  cidr_block        = "⟦cidr4:10.120.1.0/24⟧"
  ipv6_cidr_block   = "⟦cidr6:2001:db8:4a1:a00::/64⟧"
  availability_zone = "eu-central-1a"
}

resource "aws_security_group_rule" "office_ssh" {
  type              = "ingress"
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  cidr_blocks       = ["⟦cidr4:203.0.113.0/26⟧", "⟦cidr4:198.51.100.64/28⟧"]
  security_group_id = aws_security_group.bastion.id
}

resource "aws_security_group_rule" "egress_all" {
  type        = "egress"
  from_port   = 0
  to_port     = 0
  protocol    = "-1"
  cidr_blocks = ["⟦keep:0.0.0.0/0⟧"]
  security_group_id = aws_security_group.bastion.id
}

resource "aws_route53_record" "api" {
  zone_id = "Z0FAKE1234567890"
  name    = "⟦host:api.romashka.example⟧"
  type    = "A"
  ttl     = 300
  records = ["⟦ipv4:203.0.113.80⟧"]
}
