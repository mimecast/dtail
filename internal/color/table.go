package color

import (
	"fmt"
	"os"
)

const sampleParagraph string = "Mimecast is Making Email Safer for Business. We believe that securely operating a business in the cloud requires new levels of IT preparedness, centered around cyber resilience. This is why we unify the delivery and management of security, continuity and data protection for email via one, simple-to-use cloud platform. Thousands of organizations trust us to increase their cyber resilience preparedness, streamline compliance, reduce IT complexity and keep their business running. We give employees fast and secure access to sensitive business information, and ensure email keeps flowing in the event of an outage. Mimecast will remain committed to protecting your IT assets through constant innovation and focus on your success."

func TablePrintAndExit(useSampleParagraph bool) {
	for _, attr := range AttributeNames {
		if attr == "Hidden" || attr == "SlowBlink" {
			continue
		}
		printColorTable(attr, useSampleParagraph)
	}
	os.Exit(0)
}

func printColorTable(attr string, useSampleParagraph bool) {
	for _, fg := range ColorNames {
		fgColor, _ := ToFgColor(fg)
		for _, bg := range ColorNames {
			if fg == bg {
				continue
			}
			bgColor, _ := ToBgColor(bg)
			attribute, _ := ToAttribute(attr)

			text := fmt.Sprintf(" Foreground:%10s  |  Background:%10s  |  Attribute:%10s ", fg, bg, attr)
			fmt.Print(PaintStrWithAttr(text, fgColor, bgColor, attribute))
			if useSampleParagraph {
				fmt.Print("\n")
				fmt.Print(PaintStrWithAttr(sampleParagraph, fgColor, bgColor, attribute))
				fmt.Print("\n")
			}
			fmt.Print("\n")
		}
	}
}
