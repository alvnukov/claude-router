"""Django settings for the ⟦org:Romashka⟧ portal."""

SECRET_KEY = "⟦secret:django-insecure-FAKE-r0mashka-9x!k2@q#w8z$e5v^t7y&u1i*o3p(0)⟧"
DEBUG = False
ALLOWED_HOSTS = ["⟦host:portal.romashka.example⟧", "⟦host:.corp.romashka.example⟧", "⟦ipv4:10.113.8.30⟧"]
INTERNAL_IPS = ["⟦keep:127.0.0.1⟧"]

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "HOST": "⟦host:db-prod-2.corp.romashka.example⟧",
        "PORT": "5432",
        "USER": "portal",
        "PASSWORD": "⟦secret:FAKE-portal-Pa55-2026⟧",
    }
}

EMAIL_HOST = "⟦host:smtp.romashka.example⟧"
DEFAULT_FROM_EMAIL = "⟦org:Ромашка⟧ <⟦email:portal@romashka.example⟧>"
ADMINS = [("⟦person:Ivan Petrov⟧", "⟦email:ivan.petrov@romashka.example⟧")]

PRIVATE_KEY_PEM = """⟦secret:-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAAFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE
RkFLRSBrZXkgZm9yIHRlc3RzIG9ubHksIG5vdCBhIHJlYWwga2V5LCBSb21hc2hrYQAAAA==
-----END OPENSSH PRIVATE KEY-----⟧
"""
