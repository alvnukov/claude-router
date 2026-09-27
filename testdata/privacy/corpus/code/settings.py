"""Django settings for the Romashka portal."""

SECRET_KEY = "django-insecure-FAKE-r0mashka-9x!k2@q#w8z$e5v^t7y&u1i*o3p(0)"
DEBUG = False
ALLOWED_HOSTS = ["portal.romashka.example", ".corp.romashka.example", "10.113.8.30"]
INTERNAL_IPS = ["127.0.0.1"]

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "HOST": "db-prod-2.corp.romashka.example",
        "PORT": "5432",
        "USER": "portal",
        "PASSWORD": "FAKE-portal-Pa55-2026",
    }
}

EMAIL_HOST = "smtp.romashka.example"
DEFAULT_FROM_EMAIL = "Ромашка <portal@romashka.example>"
ADMINS = [("Ivan Petrov", "ivan.petrov@romashka.example")]

PRIVATE_KEY_PEM = """-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAAFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE
RkFLRSBrZXkgZm9yIHRlc3RzIG9ubHksIG5vdCBhIHJlYWwga2V5LCBSb21hc2hrYQAAAA==
-----END OPENSSH PRIVATE KEY-----
"""
