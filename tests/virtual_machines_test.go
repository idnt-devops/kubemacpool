package tests

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/k8snetworkplumbingwg/kubemacpool/pkg/names"
	pool_manager "github.com/k8snetworkplumbingwg/kubemacpool/pkg/pool-manager"
	kubevirtv1 "kubevirt.io/client-go/api/v1"
)

const timeout = 2 * time.Minute
const pollingInterval = 5 * time.Second

//TODO: the rfe_id was taken from kubernetes-nmstate we have to discover the rigth parameters here
var _ = Describe("[rfe_id:3503][crit:medium][vendor:cnv-qe@redhat.com][level:component]Virtual Machines", func() {
	BeforeAll(func() {
		result := testClient.KubeClient.ExtensionsV1beta1().RESTClient().
			Post().
			RequestURI(fmt.Sprintf(nadPostUrl, TestNamespace, "linux-bridge")).
			Body([]byte(fmt.Sprintf(linuxBridgeConfCRD, "linux-bridge", TestNamespace))).
			Do(context.TODO())
		Expect(result.Error()).NotTo(HaveOccurred(), "KubeCient should successfully respond to post request")
	})

	BeforeEach(func() {
		By("Verify that there are no VMs left from previous tests")
		currentVMList := &kubevirtv1.VirtualMachineList{}
		err := testClient.VirtClient.List(context.TODO(), currentVMList, &client.ListOptions{})
		Expect(err).ToNot(HaveOccurred(), "Should successfully list add VMs")
		Expect(len(currentVMList.Items)).To(BeZero(), "There should be no VM's in the cluster before a test")

		vmWaitConfigMap, err := testClient.KubeClient.CoreV1().ConfigMaps(managerNamespace).Get(context.TODO(), names.WAITING_VMS_CONFIGMAP, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Should successfully get %s ConfigMap", names.WAITING_VMS_CONFIGMAP))

		By(fmt.Sprintf("Clearing the map inside %s configMap in-place instead of waiting to garbage-collector", names.WAITING_VMS_CONFIGMAP))
		clearMap(vmWaitConfigMap.Data)
		vmWaitConfigMap, err = testClient.KubeClient.CoreV1().ConfigMaps(managerNamespace).Update(context.TODO(), vmWaitConfigMap, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Should successfully update %s ConfigMap", names.WAITING_VMS_CONFIGMAP))
		Expect(vmWaitConfigMap.Data).To(BeEmpty(), fmt.Sprintf("%s Data map should be empty", names.WAITING_VMS_CONFIGMAP))

		// remove all the labels from the test namespaces
		for _, namespace := range []string{TestNamespace, OtherTestNamespace} {
			err = cleanNamespaceLabels(namespace)
			Expect(err).ToNot(HaveOccurred(), "should be able to remove the namespace labels")
		}
	})

	Context("Check the client", func() {
		BeforeEach(func() {
			err := initKubemacpoolParams()
			Expect(err).ToNot(HaveOccurred())

		})
		AfterEach(func() {
			vmList := &kubevirtv1.VirtualMachineList{}
			err := testClient.VirtClient.List(context.TODO(), vmList, &client.ListOptions{})
			Expect(err).ToNot(HaveOccurred())

			for _, vmObject := range vmList.Items {
				err = testClient.VirtClient.Delete(context.TODO(), &vmObject)
				Expect(err).ToNot(HaveOccurred())
			}

			Eventually(func() int {
				vmList := &kubevirtv1.VirtualMachineList{}
				err := testClient.VirtClient.List(context.TODO(), vmList, &client.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				return len(vmList.Items)

			}, timeout, pollingInterval).Should(Equal(0), "failed to remove all vm objects")
		})

		Context("When Running with default opt-mode configuration", func() {
			BeforeEach(func() {
				By("Getting the current VM Opt-mode")
				vmWebhookOptMode, err := getOptMode(vmNamespaceOptInLabel)
				Expect(err).ToNot(HaveOccurred(), "Should succeed getting the mutating webhook opt-mode")

				if vmWebhookOptMode == optInMode {
					By("opting in the namespace")
					err := addLabelsToNamespace(TestNamespace, map[string]string{vmNamespaceOptInLabel: "allocate"})
					Expect(err).ToNot(HaveOccurred(), "should be able to add the namespace labels")
				}
			})

			Context("and the client tries to assign the same MAC address for two different vm. Within Range and out of range", func() {
				Context("When the MAC address is within range", func() {
					It("[test_id:2166]should reject a vm creation with an already allocated MAC address", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")}, []kubevirtv1.Network{newNetwork("br")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).ToNot(HaveOccurred())
						Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

						vmOverlap := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("brOverlap", "")}, []kubevirtv1.Network{newNetwork("brOverlap")})
						// Allocated the same MAC address that was registered to the first vm
						vmOverlap.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress
						err = testClient.VirtClient.Create(context.TODO(), vmOverlap)
						Expect(err).To(HaveOccurred())
						Expect(net.ParseMAC(vmOverlap.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")
					})
				})
				Context("and the MAC address is out of range", func() {
					It("[test_id:2167]should reject a vm creation with an already allocated MAC address", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "03:ff:ff:ff:ff:ff")},
							[]kubevirtv1.Network{newNetwork("br")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).ToNot(HaveOccurred())
						Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

						// Allocated the same mac address that was registered to the first vm
						vmOverlap := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("brOverlap", "03:ff:ff:ff:ff:ff")},
							[]kubevirtv1.Network{newNetwork("brOverlap")})
						err = testClient.VirtClient.Create(context.TODO(), vmOverlap)
						Expect(err).To(HaveOccurred())
						Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))

					})
				})
			})
			Context("and the client tries to assign the same MAC address for two different interfaces in a single VM.", func() {
				Context("and when the MAC address is within range", func() {
					It("[test_id:2199]should reject a VM creation with two interfaces that share the same MAC address", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", "02:00:00:00:ff:ff"),
							newInterface("br2", "02:00:00:00:ff:ff")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).To(HaveOccurred())
						Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
					})
				})
				Context("and when the MAC address is out of range", func() {
					It("[test_id:2200]should reject a VM creation with two interfaces that share the same MAC address", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", "03:ff:ff:ff:ff:ff"),
							newInterface("br2", "03:ff:ff:ff:ff:ff")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).To(HaveOccurred())
						Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
					})
				})
			})
			Context("and two VM are deleted and we try to assign their MAC addresses for two newly created VM", func() {
				It("[test_id:2164]should not return an error because the MAC addresses of the old VMs should have been released", func() {
					//creating two VMs
					vm1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", "")}, []kubevirtv1.Network{newNetwork("br1")})
					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).ToNot(HaveOccurred())
					Expect(net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

					vm2 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br2", "")}, []kubevirtv1.Network{newNetwork("br2")})
					err = testClient.VirtClient.Create(context.TODO(), vm2)
					Expect(err).ToNot(HaveOccurred())
					Expect(net.ParseMAC(vm2.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

					vm1MacAddress := vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress
					vm2MacAddress := vm2.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress

					//deleting both and try to assign their MAC address to the new VM
					deleteVMI(vm1)
					deleteVMI(vm2)

					newVM1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", vm1MacAddress)},
						[]kubevirtv1.Network{newNetwork("br1")})

					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), newVM1)
						if err != nil {
							Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
						}
						return err

					}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
					Expect(net.ParseMAC(newVM1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

					newVM2 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br2", vm2MacAddress)},
						[]kubevirtv1.Network{newNetwork("br2")})

					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), newVM2)
						if err != nil {
							Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
						}
						return err

					}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
					Expect(net.ParseMAC(newVM2.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")
				})
			})
			Context("and trying to create a VM after a MAC address has just been released due to a VM deletion", func() {
				It("[test_id:2165]should re-use the released MAC address for the creation of the new VM and not return an error", func() {
					vm1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""),
						newInterface("br2", "")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating vm1")
					mac1, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)
					Expect(err).ToNot(HaveOccurred(), "Should succeed parsing vm1's first mac")
					mac2, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)
					Expect(err).ToNot(HaveOccurred(), "Should succeed parsing vm1's second mac")

					vm2 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br3", "")},
						[]kubevirtv1.Network{newNetwork("br3")})

					err = testClient.VirtClient.Create(context.TODO(), vm2)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating vm2")
					Expect(net.ParseMAC(vm2.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm2's mac address")

					newVM1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress),
						newInterface("br2", vm1.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

					deleteVMI(vm1)

					Eventually(func() error {
						return testClient.VirtClient.Create(context.TODO(), newVM1)
					}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
					newMac1, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)
					Expect(err).ToNot(HaveOccurred(), "Should succeed parsing newVM1's first mac")
					newMac2, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)
					Expect(err).ToNot(HaveOccurred(), "Should succeed parsing newVM1's second mac")

					Expect(newMac1.String()).To(Equal(mac1.String()), "Should be equal to the first mac that was released by vm1")
					Expect(newMac2.String()).To(Equal(mac2.String()), "Should be equal to the second mac that was released by vm1")
				})
				Context("and mac-pool is full", func() {
					It("should re-use the released MAC address for the creation of the new VM and not return an error, using automatic mac allocation of the mac left on the pool", func() {
						vm1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""),
							newInterface("br2", "")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

						err := testClient.VirtClient.Create(context.TODO(), vm1)
						Expect(err).ToNot(HaveOccurred(), "Should succeed creating vm1")
						mac1, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)
						Expect(err).ToNot(HaveOccurred(), "Should succeed parsing vm1's first mac")
						mac2, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)
						Expect(err).ToNot(HaveOccurred(), "Should succeed parsing vm1's second mac")

						vm2 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br3", "")},
							[]kubevirtv1.Network{newNetwork("br3")})

						err = testClient.VirtClient.Create(context.TODO(), vm2)
						Expect(err).ToNot(HaveOccurred(), "Should succeed creating vm2")
						Expect(net.ParseMAC(vm2.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm2s's mac address")

						err = AllocateFillerVms(3)
						Expect(err).ToNot(HaveOccurred(), "Should succeed allocating all the mac pool")

						deleteVMI(vm1)

						newVM1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""),
							newInterface("br2", "")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})
						Eventually(func() error {
							return testClient.VirtClient.Create(context.TODO(), newVM1)

						}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
						newMac1, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)
						Expect(err).ToNot(HaveOccurred(), "Should succeed parsing newVM1's first mac")
						newMac2, err := net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)
						Expect(err).ToNot(HaveOccurred(), "Should succeed parsing newVM1's second mac")

						Expect(newMac1.String()).To(Equal(mac1.String()), "Should be equal to the first mac that was released by vm1")
						Expect(newMac2.String()).To(Equal(mac2.String()), "Should be equal to the second mac that was released by vm1")
					})
				})
			})
			Context("and when restarting kubeMacPool and trying to create a VM with the same manually configured MAC as an older VM", func() {
				It("[test_id:2179]should return an error because the MAC address is taken by the older VM", func() {
					vm1 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", "02:00:ff:ff:ff:ff")}, []kubevirtv1.Network{newNetwork("br1")})

					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).ToNot(HaveOccurred())
					Expect(net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")

					//restart kubeMacPool
					err = initKubemacpoolParams()
					Expect(err).ToNot(HaveOccurred())

					vm2 := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br2", "02:00:ff:ff:ff:ff")},
						[]kubevirtv1.Network{newNetwork("br2")})

					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), vm2)
						if err != nil && strings.Contains(err.Error(), "failed to allocate requested mac address") {
							return err
						}
						return nil

					}, timeout, pollingInterval).Should(HaveOccurred())
				})
			})
			Context("and we re-apply a VM yaml", func() {
				It("[test_id:2243]should assign to the VM the same MAC addresses as before the re-apply, and not return an error", func() {
					intrefaces := make([]kubevirtv1.Interface, 5)
					networks := make([]kubevirtv1.Network, 5)
					for i := 0; i < 5; i++ {
						intrefaces[i] = newInterface("br"+strconv.Itoa(i), "")
						networks[i] = newNetwork("br" + strconv.Itoa(i))
					}

					vm1 := CreateVmObject(TestNamespace, false, intrefaces, networks)
					updateObject := vm1.DeepCopy()

					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).ToNot(HaveOccurred())

					updateObject.ObjectMeta = *vm1.ObjectMeta.DeepCopy()

					for index := range vm1.Spec.Template.Spec.Domain.Devices.Interfaces {
						Expect(net.ParseMAC(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[index].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")
					}

					err = retry.RetryOnConflict(retry.DefaultRetry, func() error {

						err = testClient.VirtClient.Get(context.TODO(), client.ObjectKey{Namespace: updateObject.Namespace, Name: updateObject.Name}, updateObject)
						Expect(err).ToNot(HaveOccurred())

						err = testClient.VirtClient.Update(context.TODO(), updateObject)
						return err
					})
					Expect(err).ToNot(HaveOccurred())

					for index := range vm1.Spec.Template.Spec.Domain.Devices.Interfaces {
						Expect(vm1.Spec.Template.Spec.Domain.Devices.Interfaces[index].MacAddress).To(Equal(updateObject.Spec.Template.Spec.Domain.Devices.Interfaces[index].MacAddress))
					}
				})
			})
			Context("and we re-apply a failed VM yaml", func() {
				totalTimeout := time.Duration(0)
				BeforeAll(func() {
					vmFailCleanupWaitTime := getVmFailCleanupWaitTime()
					// since this test checks vmWaitingCleanupLook routine, we nned to adjust the total timeout with the wait-time argument.
					// we also add some extra timeout apart form wait-time to be sure that we catch the vm mac release.
					totalTimeout = timeout + vmFailCleanupWaitTime
				})
				It("[test_id:2633]should allow to assign to the VM the same MAC addresses, with name as requested before and do not return an error", func() {
					vm1 := CreateVmObject(TestNamespace, false,
						[]kubevirtv1.Interface{newInterface("br1", "02:00:ff:ff:ff:ff")},
						[]kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

					baseVM := vm1.DeepCopy()

					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).To(HaveOccurred(), "should fail to create VM due to missing interface assignment to a network")

					baseVM.Spec.Template.Spec.Domain.Devices.Interfaces = append(baseVM.Spec.Template.Spec.Domain.Devices.Interfaces, newInterface("br2", ""))

					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), baseVM)
						if err != nil {
							Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
						}
						return err

					}, totalTimeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
				})
				It("should allow to assign to the VM the same MAC addresses, different name as requested before and do not return an error", func() {
					vm1 := CreateVmObject(TestNamespace, false,
						[]kubevirtv1.Interface{newInterface("br1", "02:00:ff:ff:ff:ff")},
						[]kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

					baseVM := vm1.DeepCopy()
					baseVM.Name = "new-vm"

					err := testClient.VirtClient.Create(context.TODO(), vm1)
					Expect(err).To(HaveOccurred(), "should fail to create VM due to missing interface assignment to a network")

					baseVM.Spec.Template.Spec.Domain.Devices.Interfaces = append(baseVM.Spec.Template.Spec.Domain.Devices.Interfaces, newInterface("br2", ""))

					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), baseVM)
						if err != nil {
							Expect(strings.Contains(err.Error(), "failed to allocate requested mac address")).To(Equal(true))
						}
						return err

					}, totalTimeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")
				})
			})
			Context("and a VM's NIC is removed and a new VM is created with the same MAC", func() {
				It("[test_id:2995]should successfully release the MAC and the new VM should be created with no errors", func() {
					vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""), newInterface("br2", "")},
						[]kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})
					err := testClient.VirtClient.Create(context.TODO(), vm)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating the vm")

					Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's first mac address")
					Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's second mac address")

					reusedMacAddress := vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress
					By("checking that a new VM cannot be created when the range is full")
					newVM := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", reusedMacAddress)},
						[]kubevirtv1.Network{newNetwork("br1")})
					err = testClient.VirtClient.Create(context.TODO(), newVM)
					Expect(err).Should(MatchError("admission webhook \"mutatevirtualmachines.kubemacpool.io\" denied the request: Failed to create virtual machine allocation error: Failed to allocate mac to the vm object: failed to allocate requested mac address"), "Should fail to allocate vm because the mac is already used")

					By("checking that the VM's NIC can be removed")
					err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
						err := testClient.VirtClient.Get(context.TODO(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}, vm)
						Expect(err).ToNot(HaveOccurred(), "Should succeed getting vm")

						vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{newInterface("br2", "")}
						vm.Spec.Template.Spec.Networks = []kubevirtv1.Network{newNetwork("br2")}

						err = testClient.VirtClient.Update(context.TODO(), vm)
						return err
					})
					Expect(err).ToNot(HaveOccurred(), "should succeed to remove NIC from vm")

					Expect(len(vm.Spec.Template.Spec.Domain.Devices.Interfaces) == 1).To(Equal(true), "vm's total NICs should be 1")

					By("checking that a new VM can be created after the VM's NIC had been removed ")
					Eventually(func() error {
						err = testClient.VirtClient.Create(context.TODO(), newVM)
						if err != nil {
							Expect(err).Should(MatchError("admission webhook \"mutatevirtualmachines.kubemacpool.io\" denied the request: Failed to create virtual machine allocation error: Failed to allocate mac to the vm object: failed to allocate requested mac address"), "Should only get a mac-allocation denial error until cache get updated")
						}
						return err

					}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")

					Expect(len(newVM.Spec.Template.Spec.Domain.Devices.Interfaces) == 1).To(Equal(true), "new vm should be allocated with the now released mac address")
				})
				Context("and mac-pool is full", func() {
					It("should successfully release the MAC and the new VM should be created with no errors with the only mac left allocated to it", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""), newInterface("br2", "")},
							[]kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).ToNot(HaveOccurred(), "Should succeed creating the vm")
						Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's first mac address")
						Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's second mac address")

						err = AllocateFillerVms(2)
						Expect(err).ToNot(HaveOccurred(), "Should succeed allocating all the mac pool")

						By("checking that a new VM cannot be created when the range is full")
						newVM := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", "")},
							[]kubevirtv1.Network{newNetwork("br1")})
						err = testClient.VirtClient.Create(context.TODO(), newVM)
						Expect(err).To(HaveOccurred(), "Should fail to allocate vm because there are no free mac addresses")
						Expect(strings.Contains(err.Error(), "Failed to allocate mac to the vm object: the range is full")).To(Equal(true))

						By("checking that the VM's NIC can be removed")
						err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
							err := testClient.VirtClient.Get(context.TODO(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}, vm)
							Expect(err).ToNot(HaveOccurred(), "Should succeed getting vm")

							vm.Spec.Template.Spec.Domain.Devices.Interfaces = []kubevirtv1.Interface{newInterface("br2", "")}
							vm.Spec.Template.Spec.Networks = []kubevirtv1.Network{newNetwork("br2")}

							err = testClient.VirtClient.Update(context.TODO(), vm)
							return err
						})
						Expect(err).ToNot(HaveOccurred(), "should succeed to remove NIC from vm")

						Expect(len(vm.Spec.Template.Spec.Domain.Devices.Interfaces) == 1).To(Equal(true), "vm's total NICs should be 1")

						By("checking that a new VM can be created after the VM's NIC had been removed ")
						Eventually(func() error {
							err = testClient.VirtClient.Create(context.TODO(), newVM)
							if err != nil {
								Expect(err).Should(MatchError("Failed to create virtual machine allocation error: the range is full"), "Should only get a range full error until cache get updated")
							}
							return err

						}, timeout, pollingInterval).ShouldNot(HaveOccurred(), "failed to apply the new vm object")

						Expect(len(newVM.Spec.Template.Spec.Domain.Devices.Interfaces) == 1).To(Equal(true), "new vm should be allocated with the now released mac address")
					})
				})
			})
		})

		Context("When all the VMs are not serviced by default (opt-in mode)", func() {
			BeforeEach(func() {
				By("setting vm webhook to not accept all namespaces unless they include opt-in label")
				err := setWebhookOptMode(vmNamespaceOptInLabel, optInMode)
				Expect(err).ToNot(HaveOccurred(), "should set opt-mode to mutatingwebhookconfiguration")
			})

			Context("and kubemacpool is not opted-in on a namespace", func() {
				BeforeEach(func() {
					By("Removing any namespace labels to make sure that kubemacpool is not opted in on the namespace")
					err := cleanNamespaceLabels(TestNamespace)
					Expect(err).ToNot(HaveOccurred(), "should be able to remove the namespace labels")
				})
				It("should create a VM object without a MAC assigned", func() {
					vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
						[]kubevirtv1.Network{newNetwork("br")})

					err := testClient.VirtClient.Create(context.TODO(), vm)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating the vm")
					Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).Should(Equal(""), "should not allocated a mac to the opted-out vm")
				})
			})

			Context("and kubemacpool is opted-in on a namespace", func() {
				var vm *kubevirtv1.VirtualMachine
				BeforeEach(func() {
					By("opting in the namespace")
					err := addLabelsToNamespace(TestNamespace, map[string]string{vmNamespaceOptInLabel: "allocate"})
					Expect(err).ToNot(HaveOccurred(), "should be able to add the namespace labels")
				})
				Context("and a vm is created in the opt-in namespace", func() {
					BeforeEach(func() {
						vm = CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
							[]kubevirtv1.Network{newNetwork("br")})

						By("Create VM")
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).ToNot(HaveOccurred(), "should succeed creating the vm")
					})
					It("should create a VM object with a MAC assigned", func() {
						Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")
					})
					It("should remove the mac from vmWaitConfigMap after vm created successfully", func() {
						By("Checking vmWaitConfigMap is cleared")
						Eventually(func() map[string]string {
							data, err := getVmWaitConfigMapData()
							Expect(err).ToNot(HaveOccurred(), "Should succeed getting vmWaitConfigMap")
							return data
						}, timeout, pollingInterval).Should(BeEmpty(), "vmWaitConfigMap should be empty after successful vm creation")
					})
				})
			})
		})

		Context("When all the VMs are serviced by default (opt-out mode)", func() {
			BeforeEach(func() {
				By("setting vm webhook to accept all namespaces that don't include opt-out label")
				err := setWebhookOptMode(vmNamespaceOptInLabel, optOutMode)
				Expect(err).ToNot(HaveOccurred(), "should set opt-mode to mutatingwebhookconfiguration")
			})

			Context("and kubemacpool is opted-out on a namespace", func() {
				BeforeEach(func() {
					By("opting out the namespace")
					err := addLabelsToNamespace(TestNamespace, map[string]string{vmNamespaceOptInLabel: "ignore"})
					Expect(err).ToNot(HaveOccurred(), "should be able to add the namespace labels")
				})
				It("should create a VM object without a MAC assigned", func() {
					vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
						[]kubevirtv1.Network{newNetwork("br")})

					err := testClient.VirtClient.Create(context.TODO(), vm)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating the vm")
					Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress).Should(Equal(""), "should not allocated a mac to the opted-out vm")
				})
			})

			Context("and the client creates a vm on a non-opted-out namespace", func() {
				var vm *kubevirtv1.VirtualMachine
				BeforeEach(func() {
					vm = CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
						[]kubevirtv1.Network{newNetwork("br")})

					By("Create VM")
					err := testClient.VirtClient.Create(context.TODO(), vm)
					Expect(err).ToNot(HaveOccurred(), "should success creating the vm")
				})
				It("should remove the mac from vmWaitConfigMap after vm created successfully", func() {
					By("Checking vmWaitConfigMap is cleared")
					Eventually(func() map[string]string {
						data, err := getVmWaitConfigMapData()
						Expect(err).ToNot(HaveOccurred(), "Should succeed getting vmWaitConfigMap")
						return data
					}, timeout, pollingInterval).Should(BeEmpty(), "vmWaitConfigMap should be empty after successful vm creation")
				})
				It("should automatically assign the vm with static MAC address within range", func() {
					vmKey := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
					By("Retrieve VM")
					err := testClient.VirtClient.Get(context.TODO(), vmKey, vm)
					Expect(err).ToNot(HaveOccurred(), "should success getting the VM after creating it")

					Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse mac address")
				})
				Context("and then when we opt-out the namespace", func() {
					BeforeEach(func() {
						By("Adding namespace opt-out label to mark it as opted-out")
						err := addLabelsToNamespace(TestNamespace, map[string]string{vmNamespaceOptInLabel: "ignore"})
						Expect(err).ToNot(HaveOccurred(), "should be able to add the namespace labels")

						By("Wait 5 seconds for namespace label clean-up to be propagated at server")
						time.Sleep(5 * time.Second)
					})
					AfterEach(func() {
						By("Removing namespace opt-out label to get kmp service again")
						err := cleanNamespaceLabels(TestNamespace)
						Expect(err).ToNot(HaveOccurred(), "should be able to remove the namespace labels")

						By("Wait 5 seconds for opt-in label at namespace to be propagated at server")
						time.Sleep(5 * time.Second)
					})
					It("should able to be deleted", func() {
						By("Delete the VM after opt-out the namespace")
						deleteVMI(vm)
					})
				})
			})

			Context("and trying to create a VM and mac-pool is full", func() {
				It("should return an error because no MAC address is available", func() {
					vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br1", ""),
						newInterface("br2", "")}, []kubevirtv1.Network{newNetwork("br1"), newNetwork("br2")})

					err := testClient.VirtClient.Create(context.TODO(), vm)
					Expect(err).ToNot(HaveOccurred(), "Should succeed creating the vm")
					Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's first mac address")
					Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[1].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's second mac address")

					err = AllocateFillerVms(2)
					Expect(err).ToNot(HaveOccurred(), "Should succeed allocating all the mac pool")

					By("Trying to allocate a vm after there is no more macs to allocate")
					starvingVM := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
						[]kubevirtv1.Network{newNetwork("br")})

					err = testClient.VirtClient.Create(context.TODO(), starvingVM)
					Expect(err).To(HaveOccurred(), "Should fail to allocate vm because there are no free mac addresses")
				})
			})
			Context("and testing finalizers", func() {
				Context("When the VM is not being deleted", func() {
					It("should have a finalizer and deletion timestamp should be zero ", func() {
						vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
							[]kubevirtv1.Network{newNetwork("br")})
						err := testClient.VirtClient.Create(context.TODO(), vm)
						Expect(err).ToNot(HaveOccurred())

						Eventually(func() bool {
							err = testClient.VirtClient.Get(context.TODO(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}, vm)
							Expect(err).ToNot(HaveOccurred())
							if vm.ObjectMeta.DeletionTimestamp.IsZero() {
								if len(vm.ObjectMeta.Finalizers) == 1 {
									if strings.Compare(vm.ObjectMeta.Finalizers[0], pool_manager.RuntimeObjectFinalizerName) == 0 {
										return true
									}
								}
							}
							return false
						}, timeout, pollingInterval).Should(BeTrue())
					})
				})
			})
		})
	})
})

func newInterface(name, macAddress string) kubevirtv1.Interface {
	return kubevirtv1.Interface{
		Name: name,
		InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
			Bridge: &kubevirtv1.InterfaceBridge{},
		},
		MacAddress: macAddress,
	}
}

func newNetwork(name string) kubevirtv1.Network {
	return kubevirtv1.Network{
		Name: name,
		NetworkSource: kubevirtv1.NetworkSource{
			Multus: &kubevirtv1.MultusNetwork{
				NetworkName: name,
			},
		},
	}
}

// This function allocates vms with 1 NIC each, in order to fill the mac pool as much as needed.
func AllocateFillerVms(macsToLeaveFree int64) error {
	maxPoolSize := getMacPoolSize()
	Expect(maxPoolSize).To(BeNumerically(">", macsToLeaveFree), "max pool size must be greater than the number of macs we want to leave free")

	By(fmt.Sprintf("Allocating another %d vms until to allocate the entire mac range", maxPoolSize-macsToLeaveFree))
	var i int64
	for i = 0; i < maxPoolSize-macsToLeaveFree; i++ {
		vm := CreateVmObject(TestNamespace, false, []kubevirtv1.Interface{newInterface("br", "")},
			[]kubevirtv1.Network{newNetwork("br")})
		vm.Name = fmt.Sprintf("vm-filler-%d", i)
		err := testClient.VirtClient.Create(context.TODO(), vm)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Should successfully create filler vm %s", vm.Name))
		Expect(net.ParseMAC(vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress)).ToNot(BeEmpty(), "Should successfully parse vm's filler mac address")
	}

	return nil
}

func deleteVMI(vm *kubevirtv1.VirtualMachine) {
	By(fmt.Sprintf("Delete vm %s/%s", vm.Namespace, vm.Name))
	err := testClient.VirtClient.Delete(context.TODO(), vm)
	if apierrors.IsNotFound(err) {
		return
	}
	Expect(err).ToNot(HaveOccurred(), "should success deleting VM")

	By(fmt.Sprintf("Wait for vm %s/%s to be deleted", vm.Namespace, vm.Name))
	Eventually(func() bool {
		err = testClient.VirtClient.Get(context.TODO(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}, vm)
		if err != nil && apierrors.IsNotFound(err) {
			return true
		}
		Expect(err).ToNot(HaveOccurred(), "should success getting vm if is still there")
		return false
	}, timeout, pollingInterval).Should(BeTrue(), "should eventually fail getting vm with IsNotFound after vm deletion")
}

func clearMap(inputMap map[string]string) {
	if inputMap != nil {
		for key := range inputMap {
			delete(inputMap, key)
		}
	}
}
