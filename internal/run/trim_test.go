package run

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("trimTail", func() {
	It("returns text unchanged when it already fits", func() {
		shown, dropped := trimTail("short", 100)
		Expect(shown).To(Equal("short"))
		Expect(dropped).To(BeZero())
	})

	It("keeps the tail and reports what it cut from the front", func() {
		shown, dropped := trimTail("0123456789", 4)
		Expect(shown).To(Equal("6789"))
		Expect(dropped).To(Equal(6))
	})

	It("cuts everything when there is no budget", func() {
		shown, dropped := trimTail("0123456789", 0)
		Expect(shown).To(BeEmpty())
		Expect(dropped).To(Equal(10))
	})

	It("never splits a multi-byte rune", func() {
		text := "€€" // € is 3 bytes in UTF-8; a 4-byte budget lands inside the first
		shown, dropped := trimTail(text, 4)
		Expect(shown).To(Equal("€"))
		Expect(dropped).To(Equal(3))
		Expect([]rune(shown)).To(HaveLen(1))
	})
})

var _ = Describe("splitBudget", func() {
	It("hands back every size unchanged when the total already fits", func() {
		Expect(splitBudget([]int{10, 20, 30}, 100)).To(Equal([]int{10, 20, 30}))
	})

	It("gives a small round everything it asks for and redistributes the rest", func() {
		// One round of 5 fits under any even share; the other two, larger
		// than their share, split what is left evenly.
		result := splitBudget([]int{5, 100, 100}, 45)
		Expect(result[0]).To(Equal(5))
		Expect(result[1]).To(Equal(20))
		Expect(result[2]).To(Equal(20))
	})

	It("splits evenly among equally oversized rounds", func() {
		result := splitBudget([]int{100, 100, 100}, 30)
		Expect(result[0] + result[1] + result[2]).To(Equal(30))
		for _, share := range result {
			Expect(share).To(BeNumerically("~", 10, 1))
		}
	})

	It("gives every round under its share what it needs, and the rest to the oversized one", func() {
		result := splitBudget([]int{1000, 8, 2}, 30)
		Expect(result[1]).To(Equal(8), "fully satisfied: under any share once the smaller round settles")
		Expect(result[2]).To(Equal(2), "fully satisfied: the smallest round settles first")
		Expect(result[0]).To(Equal(20), "the oversized round takes what the small ones did not use")
	})
})
