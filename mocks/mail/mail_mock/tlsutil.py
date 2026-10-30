"""A self-signed certificate generated when a scope opens; it is never written anywhere that lasts.

Uses `cryptography` (Apache-2.0 OR BSD-3-Clause), the mock's one runtime dependency: the standard
library can serve TLS but cannot make a certificate.
"""

from __future__ import annotations

import datetime
import ipaddress
import os
import ssl
import tempfile

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

#: Names the certificate is valid for; reserved names and loopback only.
CERT_HOSTNAME = "mail-mock.acme-example.test"
CERT_ALT_NAMES = ["localhost"]
CERT_ALT_IPS = ["127.0.0.1"]
CERT_VALID_FOR = datetime.timedelta(days=1)


class ScopeCertificate:
    """The server TLS context of one scope and the certificate a client may trust."""

    def __init__(self) -> None:
        key = ec.generate_private_key(ec.SECP256R1())
        name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, CERT_HOSTNAME)])
        now = datetime.datetime.now(datetime.timezone.utc)
        alt = [x509.DNSName(CERT_HOSTNAME)] + [x509.DNSName(n) for n in CERT_ALT_NAMES]
        alt += [x509.IPAddress(ipaddress.ip_address(ip)) for ip in CERT_ALT_IPS]
        cert = (
            x509.CertificateBuilder()
            .subject_name(name)
            .issuer_name(name)
            .public_key(key.public_key())
            .serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(minutes=5))
            .not_valid_after(now + CERT_VALID_FOR)
            .add_extension(x509.SubjectAlternativeName(alt), critical=False)
            .sign(key, hashes.SHA256())
        )
        self.certificate_pem: str = cert.public_bytes(serialization.Encoding.PEM).decode("ascii")
        key_pem = key.private_bytes(
            serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()
        )
        self.context = self._load(self.certificate_pem.encode("ascii"), key_pem)

    @staticmethod
    def _load(cert_pem: bytes, key_pem: bytes) -> ssl.SSLContext:
        """Load the pair into a server context; load_cert_chain wants files, so they live for this call only."""
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        with tempfile.TemporaryDirectory(prefix="mail-mock-tls-") as tmp:
            cert_path = os.path.join(tmp, "c.pem")
            key_path = os.path.join(tmp, "k.pem")
            with open(key_path, "wb") as f:
                os.fchmod(f.fileno(), 0o600)
                f.write(key_pem)
            with open(cert_path, "wb") as f:
                f.write(cert_pem)
            context.load_cert_chain(cert_path, key_path)
        return context
