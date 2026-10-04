import type { ChipProps } from "@tscircuit/props"

const pinLabels = {
  pin1: ["ADDR"],
  pin2: ["ALERT","RDY"],
  pin3: ["GND"],
  pin4: ["AIN0"],
  pin5: ["AIN1"],
  pin6: ["AIN2"],
  pin7: ["AIN3"],
  pin8: ["VDD"],
  pin9: ["SDA"],
  pin10: ["SCL"]
} as const

const pinAttributes = {
  pin3: {requiresGround: true},
  pin8: {requiresPower: true}
} as const

export const ADS1115IDGSR = (props: ChipProps<typeof pinLabels>) => {
  return (
    <chip
      pinLabels={pinLabels}
      pinAttributes={pinAttributes}
      supplierPartNumbers={{
  "jlcpcb": [
    "C37593"
  ]
}}
      manufacturerPartNumber="ADS1115IDGSR"
      footprint="dfn10_pillpads_p0.5mm_w5.84mm_pw0.28mm_pl1.62mm_pin1location(leftside,bottom)"
      cadModel={{
        objUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C37593.obj?uuid=a8ab40c7a7c54773a59b712bf86c7131",
        stepUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C37593.step?uuid=a8ab40c7a7c54773a59b712bf86c7131",
        pcbRotationOffset: 0,
        modelOriginPosition: { x: 0, y: 0, z: 0 },
      }}
      {...props}
    />
  )
}