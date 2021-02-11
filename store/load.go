package store

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"code.cloudfoundry.org/credhub-cli/credhub"
	"github.com/emirpasic/gods/maps/treebidimap"
	"github.com/emirpasic/gods/utils"

	boshdir "github.com/cloudfoundry/bosh-cli/director"
)

func NewStore(ch *credhub.CredHub, directorClient boshdir.Director) *Store {
	return &Store{
		certs:          treebidimap.NewWith(utils.StringComparator, certByName),
		certVersions:   treebidimap.NewWith(utils.StringComparator, certVersionById),
		deployments:    treebidimap.NewWith(utils.StringComparator, deploymentByName),
		credhubClient:  ch,
		directorClient: directorClient,
	}
}

func (s *Store) Refresh() error {
	certs, err := s.credhubClient.GetAllCertificatesMetadata()
	if err != nil {
		return err
	}

	for _, certMeta := range certs {
		cert := Cert{
			Id:   certMeta.Id,
			Name: certMeta.Name,
		}

		versions := make([]*CertVersion, 0)
		for _, certMetaVersion := range certMeta.Versions {
			expiry, err := time.Parse(time.RFC3339, certMetaVersion.ExpiryDate)
			if err != nil {
				return fmt.Errorf("failed to parse expiry date: %s for cert version: %s",
					certMetaVersion.ExpiryDate, certMetaVersion.Id)
			}
			cv := CertVersion{
				Id:                   certMetaVersion.Id,
				Cert:                 &cert,
				Transitional:         certMetaVersion.Transitional,
				CertificateAuthority: certMetaVersion.CertificateAuthority,
				SelfSigned:           certMetaVersion.SelfSigned,
				Expiry:               expiry,
				Deployments:          make([]*Deployment, 0),
			}
			versions = append(versions, &cv)
			s.certVersions.Put(cv.Id, &cv)
		}
		cert.Versions = versions
		s.certs.Put(certMeta.Name, &cert)
	}

	// for each certMeta fetch raw cert + ca and decode with x509
	for _, certMeta := range certs {
		credentials, err := s.credhubClient.GetAllVersions(certMeta.Name)
		if err != nil {
			return err
		}

		for _, c := range credentials {
			if c.Base.Type == "certificate" {
				raw := c.Value.(map[string]interface{})
				rawCert := raw["certificate"].(string)

				certBlock, _ := pem.Decode([]byte(rawCert))
				certificate, err := x509.ParseCertificate(certBlock.Bytes)
				if err != nil {
					return fmt.Errorf("failed to parse certificate: %s", err)
				}

				cv, _ := s.certVersions.Get(c.Base.Id)
				certVersion := cv.(*CertVersion)
				certVersion.Certificate = certificate
			}
		}
	}

	// Lookup Ca for each cert
	it := s.certVersions.Iterator()
	for it.End(); it.Prev(); {
		_, value := it.Key(), it.Value()
		v := value.(*CertVersion)
		authorityKeyID := v.Certificate.AuthorityKeyId
		if v.SelfSigned {
			continue
		}
		ca, found := s.getCertVersionBySubjectKeyId(authorityKeyID)
		if found {
			ca.Signs = append(ca.Signs, v)
			v.SignedBy = ca
		} else {
			return fmt.Errorf("failed to lookup ca CertVersion with id: %s", v.Id)
		}
	}

	deployments, err := s.directorClient.Deployments()
	if err != nil {
		return err
	}
	for _, deployment := range deployments {
		d := Deployment{
			Name:     deployment.Name(),
			Versions: make([]*CertVersion, 0),
		}
		s.deployments.Put(d.Name, &d)
		variables, err := deployment.Variables()
		if err != nil {
			return err
		}
		for _, variable := range variables {
			cv, _ := s.certVersions.Get(variable.ID)
			if cv == nil {
				continue
			}
			certVersion := cv.(*CertVersion)
			certVersion.Deployments = append(certVersion.Deployments, &d)
			d.Versions = append(d.Versions, certVersion)
		}
	}

	return nil
}
