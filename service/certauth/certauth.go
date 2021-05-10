// Package certauth
package certauth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jessepeterson/nanomdm/log"
	"github.com/jessepeterson/nanomdm/mdm"
	"github.com/jessepeterson/nanomdm/service"
	"github.com/jessepeterson/nanomdm/storage"
)

var (
	ErrNoCertReuse = errors.New("cert re-use not permitted")
	ErrNoCertAssoc = errors.New("enrollment not associated with cert")
	ErrMissingCert = errors.New("missing MDM certificate")
)

// normalize pulls out only the "device" ID (i.e. the "parent" of the)
// MDM relationship regardless of enrollment type.
func normalize(e *mdm.Enrollment) *mdm.EnrollID {
	r := e.Resolved()
	if r == nil {
		return nil
	}
	return &mdm.EnrollID{
		ID:   r.DeviceChannelID,
		Type: r.Type,
	}
}

type CertAuth struct {
	next       service.CheckinAndCommandService
	logger     log.Logger
	normalizer func(e *mdm.Enrollment) *mdm.EnrollID
	storage    storage.CertAuthStore

	// allowDup potentially allows duplicate certificates to be used
	// for more than one enrollment. This may be permissible if, say,
	// a shared embedded identity is used in the enrollment profile.
	// otherwise, for SCEP, this should never happen: every enrollment
	// should be a uniquely issued certificate.
	allowDup bool

	// allowRetroactive allows cert-hash associations to happen in
	// requests other than the Authenticate Check-in message. If this is
	// true then you can effectively add cert-hash associations to
	// existing enrollments and not just new enrollments. However,
	// if the enrollment has an existing association we explicitly
	// disallow re-association.
	allowRetroactive bool

	// warnOnly won't return an error when we find a cert auth problem
	// and we won't save associations for existing enrollments.
	// This can be used to troubleshoot or remediate enrollments that
	// are having problems with associations by e.g. sending them a
	// enrollment profile via the MDM channel.
	//
	// WARNING: This allows MDM clients to spoof other MDM clients.
	warnOnly bool
}

func NewCertAuthMiddleware(next service.CheckinAndCommandService, storage storage.CertAuthStore, logger log.Logger) *CertAuth {
	return &CertAuth{
		next:       next,
		logger:     logger,
		normalizer: normalize,
		storage:    storage,
	}
}

func hashCert(cert *x509.Certificate) string {
	hashed := sha256.Sum256(cert.Raw)
	b := make([]byte, len(hashed))
	copy(b, hashed[:])
	return hex.EncodeToString(b)
}

func (s *CertAuth) associateNewEnrollment(r *mdm.Request) error {
	if r.Certificate == nil {
		return ErrMissingCert
	}
	if err := r.EnrollID.Validate(); err != nil {
		return err
	}
	hash := hashCert(r.Certificate)
	if hasHash, err := s.storage.HasCertHash(r, hash); err != nil {
		return err
	} else if hasHash {
		if !s.allowDup {
			// test to see if we're using the same cert for an
			// enrollment. the only way this should happen is if
			// the cert is embedded in the profile and they're re-using
			// the cert. permit this one case.
			if isAssoc, err := s.storage.IsCertHashAssociated(r, hash); err != nil {
				return err
			} else if isAssoc {
				return nil
			}
			s.logger.Info(
				"msg", "cert hash exists",
				"enrollment", "new",
				"id", r.ID,
				"hash", hash,
			)
			if !s.warnOnly {
				return ErrNoCertReuse
			}
		}
	}
	if err := s.storage.AssociateCertHash(r, hash); err != nil {
		return err
	}
	s.logger.Info(
		"msg", "cert associated",
		"enrollment", "new",
		"id", r.ID,
		"hash", hash,
	)
	return nil
}

func (s *CertAuth) validateAssociateExistingEnrollment(r *mdm.Request) error {
	if r.Certificate == nil {
		return ErrMissingCert
	}
	if err := r.EnrollID.Validate(); err != nil {
		return err
	}
	hash := hashCert(r.Certificate)
	if isAssoc, err := s.storage.IsCertHashAssociated(r, hash); err != nil {
		return err
	} else if isAssoc {
		return nil
	}
	if !s.allowRetroactive {
		s.logger.Info(
			"msg", "no cert association",
			"enrollment", "existing",
			"id", r.ID,
			"hash", hash,
		)
		if !s.warnOnly {
			return ErrNoCertAssoc
		}
	}
	// even if allowRetroactive is true we don't want to allow arbitrary
	// existing enrollments to use a different association. you must
	// MDM re-Authenticate first. so we check that this enrollment
	// has no association yet.
	if hasHash, err := s.storage.EnrollmentHasCertHash(r, hash); err != nil {
		return err
	} else if hasHash {
		s.logger.Info(
			"msg", "enrollment cannot have associated cert hash",
			"enrollment", "existing",
			"id", r.ID,
		)
		if !s.warnOnly {
			return ErrNoCertReuse
		}
	}
	// even if allowDup were true we don't want to allow arbitrary
	// existing enrollments to use another association. you must
	// MDM re-Authenticate first. so we check that this cert hasn't
	// been seen before to prevent any possible exfiltrated cert
	// connections.
	if hasHash, err := s.storage.HasCertHash(r, hash); err != nil {
		return err
	} else if hasHash {
		s.logger.Info(
			"msg", "cert hash exists",
			"enrollment", "existing",
			"id", r.ID,
			"hash", hash,
		)
		if !s.warnOnly {
			return ErrNoCertReuse
		}
	}
	if s.warnOnly {
		return nil
	}
	if err := s.storage.AssociateCertHash(r, hash); err != nil {
		return err
	}
	s.logger.Info(
		"msg", "cert associated",
		"enrollment", "existing",
		"id", r.ID,
		"hash", hash,
	)
	return nil
}

func (s *CertAuth) Authenticate(r *mdm.Request, m *mdm.Authenticate) error {
	req := r.Clone()
	req.EnrollID = s.normalizer(&m.Enrollment)
	if err := s.associateNewEnrollment(req); err != nil {
		return fmt.Errorf("cert auth: new enrollment: %w", err)
	}
	return s.next.Authenticate(r, m)
}

func (s *CertAuth) TokenUpdate(r *mdm.Request, m *mdm.TokenUpdate) error {
	req := r.Clone()
	req.EnrollID = s.normalizer(&m.Enrollment)
	err := s.validateAssociateExistingEnrollment(req)
	if err != nil {
		return fmt.Errorf("cert auth: existing enrollment: %w", err)
	}
	return s.next.TokenUpdate(r, m)
}

func (s *CertAuth) CheckOut(r *mdm.Request, m *mdm.CheckOut) error {
	req := r.Clone()
	req.EnrollID = s.normalizer(&m.Enrollment)
	err := s.validateAssociateExistingEnrollment(req)
	if err != nil {
		return fmt.Errorf("cert auth: existing enrollment: %w", err)
	}
	return s.next.CheckOut(r, m)
}

func (s *CertAuth) CommandAndReportResults(r *mdm.Request, results *mdm.CommandResults) (*mdm.Command, error) {
	req := r.Clone()
	req.EnrollID = s.normalizer(&results.Enrollment)
	if err := s.validateAssociateExistingEnrollment(req); err != nil {
		return nil, fmt.Errorf("cert auth: existing enrollment: %w", err)
	}
	return s.next.CommandAndReportResults(r, results)
}
