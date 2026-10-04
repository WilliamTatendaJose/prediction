import type { ChipProps } from "@tscircuit/props"

const pinLabels = {
  pin1: ["pin1"],
  pin2: ["pin2"],
  pin3: ["pin3"],
  pin4: ["pin4"]
} as const

export const EL357N_C__TA__G = (props: ChipProps<typeof pinLabels>) => {
  return (
    <chip
      pinLabels={pinLabels}
      symbol={
        <symbol>
          <schematicpath points={[{"x":-0.6,"y":0.2},{"x":-0.34,"y":0.2},{"x":-0.34,"y":-0.2},{"x":-0.6,"y":-0.2}]} strokeColor="#880000" />
          <schematicpath svgPath="M -0.46 0.1 L -0.34 -0.1 L -0.2 0.1 Z" strokeColor="#880000" isFilled fillColor="#880000" />
          <schematicpath points={[{"x":-0.18,"y":-0.1},{"x":-0.5,"y":-0.1}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.12,"y":0.06},{"x":0.02,"y":-0.08}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.18,"y":-0.02},{"x":-0.04,"y":-0.16}]} strokeColor="#880000" />
          <schematicpath svgPath="M 0.02 -0.08 L -0.02 0 L -0.06 -0.04 Z" strokeColor="#880000" isFilled fillColor="#880000" />
          <schematicpath svgPath="M -0.04 -0.16 L -0.08 -0.08 L -0.12 -0.12 Z" strokeColor="#880000" isFilled fillColor="#880000" />
          <schematicpath points={[{"x":0.4,"y":-0.2},{"x":0.34,"y":-0.1},{"x":0.28,"y":-0.18},{"x":0.4,"y":-0.2}]} strokeColor="#880000" isFilled fillColor="#880000" />
          <schematicpath points={[{"x":0.2,"y":0.18},{"x":0.2,"y":-0.18}]} strokeColor="#880000" />
          <schematicpath points={[{"x":0.2,"y":-0.06},{"x":0.4,"y":-0.2}]} strokeColor="#880000" />
          <schematicpath points={[{"x":0.4,"y":0.2},{"x":0.2,"y":0.06}]} strokeColor="#880000" />
          <port name="pin4" pinNumber={4} aliases={["4"]} direction="right" schX={0.8} schY={0.2} schStemLength={0.4} />
          <port name="pin3" pinNumber={3} aliases={["3"]} direction="right" schX={0.8} schY={-0.2} schStemLength={0.4} />
          <port name="pin2" pinNumber={2} aliases={["2"]} direction="left" schX={-1} schY={-0.2} schStemLength={0.4} />
          <port name="pin1" pinNumber={1} aliases={["1"]} direction="left" schX={-1} schY={0.2} schStemLength={0.4} />
          <schematicrect schX={-0.1} schY={0} width={1} height={0.68} strokeWidth={0.02} color="#880000" />
        </symbol>
      }
      supplierPartNumbers={{
  "jlcpcb": [
    "C42379244"
  ]
}}
      manufacturerPartNumber="EL357N(C)(TA)-G"
      footprint="dfn4_p2.54mm_w8.2999mm_pw0.95mm_pl1.7mm"
      cadModel={{
        objUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C42379244.obj?uuid=c4a706bf43454f48bfde2d8029e8561c",
        stepUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C42379244.step?uuid=c4a706bf43454f48bfde2d8029e8561c",
        pcbRotationOffset: 0,
        modelOriginPosition: { x: 0, y: 0.00011429999999279516, z: 0.115963 },
      }}
      {...props}
    />
  )
}