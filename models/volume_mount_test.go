package models_test

import (
	"encoding/json"

	"code.cloudfoundry.org/bbs/models"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("VolumeMount", func() {
	Context("Validate", func() {
		var (
			mount models.VolumeMount
			err   error
		)

		BeforeEach(func() {
			mount = models.VolumeMount{
				Driver:       "my-driver",
				ContainerDir: "/mnt/mypath",
				Mode:         "r",
				Device: &models.VolumeMount_Shared{
					Shared: &models.SharedDevice{
						VolumeId:    "my-volume",
						MountConfig: `{"foo":"bar"}`,
					},
				},
			}
		})

		JustBeforeEach(func() {
			err = mount.Validate()
		})

		It("doesnt error with a good mount", func() {
			Expect(err).NotTo(HaveOccurred())
		})

		Context("given an invalid, deprecated config", func() {
			BeforeEach(func() {
				mount.DeprecatedConfig = []byte("wat")
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		Context("given an invalid, deprecated volumeId", func() {
			BeforeEach(func() {
				mount.DeprecatedVolumeId = "badness"
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		Context("given an invalid driver", func() {
			BeforeEach(func() {
				mount.Driver = ""
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		Context("given an invalid volumeId", func() {
			BeforeEach(func() {
				mount.GetShared().VolumeId = ""
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		Context("given an unset mode", func() {
			BeforeEach(func() {
				mount.Mode = ""
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})
		Context("marshall JSON", func() {
			FIt("does not eturn an error on marshal unmarshal", func() {
				data, err := json.Marshal(mount)
				Expect(err).NotTo(HaveOccurred())
				var newMount models.VolumeMount
				err = json.Unmarshal(data, &newMount)
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

})
